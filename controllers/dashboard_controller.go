/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	stderr "errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/grafana-operator/grafana-operator-experimental/api/v1beta1"
	client2 "github.com/grafana-operator/grafana-operator-experimental/controllers/client"
	"github.com/grafana-operator/grafana-operator-experimental/controllers/fetchers"
	"github.com/grafana-operator/grafana-operator-experimental/controllers/metrics"
	grapi "github.com/grafana/grafana-api-golang-client"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strconv"
	"time"
)

const (
	initialSyncDelay = "10s"
	syncBatchSize    = 100
)

// GrafanaDashboardReconciler reconciles a GrafanaDashboard object
type GrafanaDashboardReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Discovery discovery.DiscoveryInterface
}

//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards/finalizers,verbs=update

func (r *GrafanaDashboardReconciler) syncDashboards(ctx context.Context) (ctrl.Result, error) {
	syncLog := log.FromContext(ctx)
	dashboardsSynced := 0

	// get all grafana instances
	grafanas := &v1beta1.GrafanaList{}
	var opts []client.ListOption
	err := r.Client.List(ctx, grafanas, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// no instances, no need to sync
	if len(grafanas.Items) == 0 {
		return ctrl.Result{Requeue: false}, nil
	}

	// get all dashboards
	allDashboards := &v1beta1.GrafanaDashboardList{}
	err = r.Client.List(ctx, allDashboards, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// sync dashboards, delete dashboards from grafana that do no longer have a cr
	dashboardsToDelete := map[*v1beta1.Grafana][]v1beta1.NamespacedResource{}
	for _, grafana := range grafanas.Items {
		for _, dashboard := range grafana.Status.Dashboards {
			if allDashboards.Find(dashboard.Namespace(), dashboard.Name()) == nil {
				dashboardsToDelete[&grafana] = append(dashboardsToDelete[&grafana], dashboard)
			}
		}
	}

	// delete all dashboards that no longer have a cr
	for grafana, dashboards := range dashboardsToDelete {
		grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, grafana)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		for _, dashboard := range dashboards {
			// avoid bombarding the grafana instance with a large number of requests at once, limit
			// the sync to a certain number of dashboards per cycle. This means that it will take longer to sync
			// a large number of deleted dashboard crs, but that should be an edge case.
			if dashboardsSynced >= syncBatchSize {
				return ctrl.Result{Requeue: true}, nil
			}

			namespace, name, uid := dashboard.Split()
			err = grafanaClient.DeleteDashboardByUID(uid)
			if err != nil {
				return ctrl.Result{Requeue: false}, err
			}

			grafana.Status.Dashboards = grafana.Status.Dashboards.Remove(namespace, name)
			dashboardsSynced += 1
		}

		// one update per grafana - this will trigger a reconcile of the grafana controller
		// so we should minimize those updates
		err = r.Client.Status().Update(ctx, grafana)
		if err != nil {
			return ctrl.Result{Requeue: false}, err
		}
	}

	if dashboardsSynced > 0 {
		syncLog.Info("successfully synced dashboards", "dashboards", dashboardsSynced)
	}
	return ctrl.Result{Requeue: false}, nil
}

func (r *GrafanaDashboardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controllerLog := log.FromContext(ctx)
	r.Log = controllerLog

	// periodic sync reconcile
	if req.Namespace == "" && req.Name == "" {
		start := time.Now()
		syncResult, err := r.syncDashboards(ctx)
		elapsed := time.Since(start).Milliseconds()
		metrics.InitialDashboardSyncDuration.Set(float64(elapsed))
		return syncResult, err
	}

	dashboard := &v1beta1.GrafanaDashboard{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, dashboard)
	if err != nil {
		if errors.IsNotFound(err) {
			err = r.onDashboardDeleted(ctx, req.Namespace, req.Name)
			if err != nil {
				return ctrl.Result{RequeueAfter: RequeueDelayError}, err
			}
			return ctrl.Result{}, nil
		}
		controllerLog.Error(err, "error getting grafana dashboard cr")
		return ctrl.Result{RequeueAfter: RequeueDelayError}, err
	}

	// skip dashboards without an instance selector
	if dashboard.Spec.InstanceSelector == nil {
		controllerLog.Info("no instance selector found for dashboard, nothing to do", "name", dashboard.Name, "namespace", dashboard.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, nil
	}

	instances, err := GetMatchingInstances(ctx, r.Client, dashboard.Spec.InstanceSelector)
	if err != nil {
		controllerLog.Error(err, "could not find matching instance", "name", dashboard.Name)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, err
	}

	if len(instances.Items) == 0 {
		controllerLog.Info("no matching instances found for dashboard", "dashboard", dashboard.Name, "namespace", dashboard.Namespace)

		// TODO when a label selector has been updated to no longer match any Grafana instances, should we delete the dashboard from those instances?
		return ctrl.Result{Requeue: false}, nil
	}

	controllerLog.Info("found matching Grafana instances for dashboard", "count", len(instances.Items))

	success := true
	for _, grafana := range instances.Items {
		// an admin url is required to interact with grafana
		// the instance or route might not yet be ready
		//if grafana.Status.AdminUrl == "" || grafana.Status.Stage != v1beta1.OperatorStageComplete || grafana.Status.StageStatus != v1beta1.OperatorStageResultSuccess {
		if grafana.Status.Stage != v1beta1.OperatorStageComplete || grafana.Status.StageStatus != v1beta1.OperatorStageResultSuccess {
			controllerLog.Info("grafana instance not ready", "grafana", grafana.Name)
			success = false
			continue
		}

		// first reconcile the plugins
		// append the requested dashboards to a configmap from where the
		// grafana reconciler will pick them up
		err = ReconcilePlugins(ctx, r.Client, r.Scheme, &grafana, dashboard.Spec.Plugins, fmt.Sprintf("%v-dashboard", dashboard.Name))
		if err != nil {
			controllerLog.Error(err, "error reconciling plugins", "dashboard", dashboard.Name, "grafana", grafana.Name)
			success = false
		}

		// then import the dashboard into the matching grafana instances
		err = r.onDashboardCreated(ctx, &grafana, dashboard)
		if err != nil {
			controllerLog.Error(err, "error reconciling dashboard", "dashboard", dashboard.Name, "grafana", grafana.Name)
			success = false
		}
	}

	// if the dashboard was successfully synced in all instances, wait for its re-sync period
	if success {
		return ctrl.Result{RequeueAfter: dashboard.GetResyncPeriod()}, nil
	}

	return ctrl.Result{RequeueAfter: RequeueDelayError}, nil
}

func (r *GrafanaDashboardReconciler) onDashboardDeleted(ctx context.Context, namespace string, name string) error {
	list := v1beta1.GrafanaList{}
	var opts []client.ListOption
	err := r.Client.List(ctx, &list, opts...)
	if err != nil {
		return err
	}

	for _, grafana := range list.Items {
		if found, uid := grafana.Status.Dashboards.Find(namespace, name); found {
			grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, &grafana)
			if err != nil {
				return err
			}

			dash, err := grafanaClient.DashboardByUID(*uid)
			if err != nil {
				if !strings.Contains(err.Error(), "status: 404") {
					return err
				}
			}
			folderID := dash.Folder

			err = grafanaClient.DeleteDashboardByUID(*uid)
			if err != nil {
				if !strings.Contains(err.Error(), "status: 404") {
					return err
				}
			}

			resp, err := r.DeleteFolderIfEmpty(grafanaClient, folderID)
			if err != nil {
				return err
			}
			if resp.StatusCode == 200 {
				r.Log.Info("unused folder successfully removed")
			}
			if resp.StatusCode == 432 {
				r.Log.Info("folder still in use by other dashboards")
			}

			err = ReconcilePlugins(ctx, r.Client, r.Scheme, &grafana, nil, fmt.Sprintf("%v-dashboard", name))
			if err != nil {
				return err
			}

			grafana.Status.Dashboards = grafana.Status.Dashboards.Remove(namespace, name)
			err = r.Client.Status().Update(ctx, &grafana)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *GrafanaDashboardReconciler) onDashboardCreated(ctx context.Context, grafana *v1beta1.Grafana, cr *v1beta1.GrafanaDashboard) error {
	dashboardJson, err := r.fetchDashboardJson(cr)
	if err != nil {
		return err
	}

	// Dashboards come from different sources, whereas Spec.Json is used to calculate hash
	// So, we should keep the field updated to make sure changes in dashboards get noticed
	cr.Spec.Json = string(dashboardJson)

	grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, grafana)
	if err != nil {
		return err
	}

	// update/create the dashboard if it doesn't exist in the instance or has been changed
	exists, err := r.Exists(grafanaClient, cr)
	if err != nil {
		return err
	}
	if exists && cr.Unchanged() {
		return nil
	}

	var dashboardFromJson map[string]interface{}
	err = json.Unmarshal(dashboardJson, &dashboardFromJson)
	if err != nil {
		return err
	}

	folderID, err := r.GetOrCreateFolder(grafanaClient, cr)
	if err != nil {
		return errors.NewInternalError(err)
	}

	dashboardFromJson["uid"] = string(cr.UID)
	resp, err := grafanaClient.NewDashboard(grapi.Dashboard{
		Meta: grapi.DashboardMeta{
			IsStarred: false,
			Slug:      cr.Name,
			Folder:    folderID,
			// URL:       "",
		},
		Model:     dashboardFromJson,
		Overwrite: true,
		Message:   "",
	})
	if err != nil {
		return err
	}

	if resp.Status != "success" {
		return errors.NewBadRequest(fmt.Sprintf("error creating dashboard, status was %v", resp.Status))
	}

	grafana.Status.Dashboards = grafana.Status.Dashboards.Add(cr.Namespace, cr.Name, resp.UID)
	err = r.Client.Status().Update(ctx, grafana)
	if err != nil {
		return err
	}

	return r.UpdateStatus(ctx, cr)
}

// fetchDashboardJson delegates obtaining the dashboard json definition to one of the known fetchers, for example
// from embedded raw json or from a url
func (r *GrafanaDashboardReconciler) fetchDashboardJson(dashboard *v1beta1.GrafanaDashboard) ([]byte, error) {
	sourceTypes := dashboard.GetSourceTypes()

	if len(sourceTypes) == 0 {
		return nil, stderr.New(fmt.Sprintf("no source type provided for dashboard %v", dashboard.Name))
	}

	if len(sourceTypes) > 1 {
		return nil, stderr.New(fmt.Sprintf("more than one source types found for dashboard %v", dashboard.Name))
	}

	switch sourceTypes[0] {
	case v1beta1.DashboardSourceTypeRawJson:
		return []byte(dashboard.Spec.Json), nil
	case v1beta1.DashboardSourceTypeUrl:
		return fetchers.FetchDashboardFromUrl(dashboard)
	default:
		return nil, stderr.New(fmt.Sprintf("unknown source type %v found in dashboard %v", sourceTypes[0], dashboard.Name))
	}
}

func (r *GrafanaDashboardReconciler) UpdateStatus(ctx context.Context, cr *v1beta1.GrafanaDashboard) error {
	cr.Status.Hash = cr.Hash()
	return r.Client.Status().Update(ctx, cr)
}

func (r *GrafanaDashboardReconciler) Exists(client *grapi.Client,
	cr *v1beta1.GrafanaDashboard) (bool, error) {
	dashboards, err := client.Dashboards()
	if err != nil {
		return false, err
	}
	for _, dashboard := range dashboards {
		if dashboard.UID == string(cr.UID) {
			return true, nil
		}
	}
	return false, nil
}

func (r *GrafanaDashboardReconciler) GetOrCreateFolder(client *grapi.Client, cr *v1beta1.GrafanaDashboard) (int64, error) {
	if cr.Spec.FolderTitle == "" {
		return 0, nil
	}

	folderID, err := r.GetFolderID(client, cr)
	if err != nil {
		return 0, err
	}
	if folderID != 0 {
		return folderID, nil
	}

	// Folder wasn't found, let's create it
	resp, err := client.NewFolder(cr.Spec.FolderTitle)
	if err != nil {
		return 0, err
	}
	return resp.ID, nil
}

func (r *GrafanaDashboardReconciler) GetFolderID(client *grapi.Client,
	cr *v1beta1.GrafanaDashboard) (int64, error) {
	folders, err := client.Folders()
	if err != nil {
		return 0, err
	}

	for _, folder := range folders {
		if folder.Title == cr.Spec.FolderTitle {
			return folder.ID, nil
		}
		continue
	}
	return 0, nil
}

func (r *GrafanaDashboardReconciler) DeleteFolderIfEmpty(client *grapi.Client,
	folderID int64) (http.Response, error) {

	dashboards, err := client.Dashboards()
	if err != nil {
		return http.Response{
			Status:     "internal grafana client error getting dashboards",
			StatusCode: 500,
		}, err
	}

	for _, dashboard := range dashboards {
		if int64(dashboard.FolderID) == folderID {
			return http.Response{
				Status:     "resource is still in use",
				StatusCode: 423, //Locked return code
			}, err
		}
		continue
	}

	if err = client.DeleteFolder(strconv.FormatInt(folderID, 10)); err != nil {
		return http.Response{
			Status:     "internal grafana client error deleting grafana folder",
			StatusCode: 500,
		}, err
	}
	return http.Response{
		Status:     "grafana folder deleted",
		StatusCode: 200,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GrafanaDashboardReconciler) SetupWithManager(mgr ctrl.Manager, stop chan bool) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.GrafanaDashboard{}).
		Complete(r)

	if err == nil {
		d, err := time.ParseDuration(initialSyncDelay)
		if err != nil {
			return err
		}

		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(d):
					result, err := r.Reconcile(context.Background(), ctrl.Request{})
					if err != nil {
						r.Log.Error(err, "error synchronizing dashboards")
						continue
					}
					if result.Requeue {
						r.Log.Info("more dashboards left to synchronize")
						continue
					}
					r.Log.Info("dashboard sync complete")
					return
				}
			}
		}()
	}

	return err
}
