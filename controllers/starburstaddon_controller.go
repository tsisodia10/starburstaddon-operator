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
	"fmt"
	"time"

	addonv1alpha1 "github.com/cmwylie19/addon-operator/api/v1alpha1"
	"github.com/go-logr/logr"

	configv1 "github.com/openshift/api/config/v1"
	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// AddonReconciler reconciles a Addon object
type AddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

var (
	Name = "starburst"
	// Namespace = "addon-operator-system"
	Namespace = "redhat-starburst-operator"
)

// +kubebuilder:rbac:groups=charts.starburstdata.com,resources=starburstenterprises,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=addon.redhat.com,resources=addons,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=addon.redhat.com,resources=addons/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=addon.redhat.com,resources=addons/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources={alertmanagers,prometheuses,alertmanagerconfigs},verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=podmonitors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;update;patch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Addon object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *AddonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Addon instance
	// if addon is not found, we will ignore it will stop reconciling
	addon := &addonv1alpha1.Addon{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      req.Name,
		Namespace: req.Namespace,
	}, addon); err != nil {

		if k8serrors.IsNotFound(err) {
			logger.Info("Addon CR not found. Probably deleted.")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("could not get Addon CR: %v", err)
	}

	// Fetch clusterversion instance
	cv := &configv1.ClusterVersion{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      req.Name,
		Namespace: req.Namespace,
	}, cv); err != nil {

		if k8serrors.IsNotFound(err) {
			logger.Info("ClusterVersion not found")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("could not get ClusterVersion CR: %v", err)
	}

	// Fetch User Params Secret
	userParams := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "addon-managed-starburst-parameters",
		Namespace: req.Namespace,
	}, userParams); err != nil {

		if k8serrors.IsNotFound(err) {
			logger.Info("User Params Secret not found.")
			return ctrl.Result{Requeue: true}, nil
		}

		return ctrl.Result{Requeue: true}, fmt.Errorf("could not get User Params Secret: %v", err)
	}

	// Get or Create starburst-license secret
	licenseSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "starburst-license",
		Namespace: req.Namespace,
	}, licenseSecret); err != nil && k8serrors.IsNotFound(err) {
		logger.Info("License Secret not found. Creating...")
		licenseSecret = r.DeployLicenseSecret(ctx, string(userParams.Data["starburst-license"]), req.Namespace)
		if err := r.Client.Create(ctx, licenseSecret); err != nil {
			return ctrl.Result{Requeue: true}, fmt.Errorf("could not create License Secret: %v", err)
		}

		// License secret creation successful
		// We will requeue the reconciliation so that we can ensure the secret remains
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "could not get License Secret")
		return ctrl.Result{Requeue: true}, fmt.Errorf("could not get License Secret: %v", err)
	}

	// Secret
	vault := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      "addon",
		Namespace: req.Namespace,
	}, vault); err != nil {

		if k8serrors.IsNotFound(err) {
			logger.Info("Addon Secret not found.")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, fmt.Errorf("could not get Addon Secret: %v", err)
	}

	// Deploy Prometheus
	prometheus := &promv1.Prometheus{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      Name,
		Namespace: Namespace,
	}, prometheus); err != nil && k8serrors.IsNotFound(err) {
		logger.Info("Prometheus not found. Creating...")
		// tokenURL, remoteWriteURL, clusterID string
		prometheus = r.DeployPrometheus(string(vault.Data["token-url"]), string(vault.Data["remote-write-url"]), fetchClusterID())
		if err := r.Client.Create(ctx, prometheus); err != nil {
			logger.Error(err, "Could not create Prometheus")
			return ctrl.Result{Requeue: true}, fmt.Errorf("could not create Prometheus: %v", err)
		}

		// Prometheus created successfully
		// We will requeue the request to ensure the Prometheus is created
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		logger.Error(err, "could not get Prometheus")
		// return the error for the next reconcile
		return ctrl.Result{Requeue: true}, fmt.Errorf("could not get Prometheus: %v", err)
	}

	// Deploy ServiceMonitor
	serviceMonitor := &promv1.ServiceMonitor{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      Name,
		Namespace: Namespace,
	}, serviceMonitor); err != nil && k8serrors.IsNotFound(err) {
		logger.Info("Service Monitor not found. Creating...")
		serviceMonitor = r.DeployServiceMonitor()
		if err := r.Client.Create(ctx, serviceMonitor); err != nil {
			logger.Error(err, "Could not create Service Monitor")
			return ctrl.Result{Requeue: true}, fmt.Errorf("could not create service monitor: %v", err)
		}
	}

	// Deploy Federation ServiceMonitor
	fedServiceMonitor := &promv1.ServiceMonitor{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      Name + "-federation",
		Namespace: Namespace,
	}, fedServiceMonitor); err != nil && k8serrors.IsNotFound(err) {
		logger.Info("Federation Service Monitor not found. Creating...")
		fedServiceMonitor = r.DeployFederationServiceMonitor()
		if err := r.Client.Create(ctx, fedServiceMonitor); err != nil {
			logger.Error(err, "Could not create Federation Service Monitor")
			return ctrl.Result{Requeue: true}, fmt.Errorf("could not create federation service monitor: %v", err)
		}
	}

	// Deploy PrometheusRules
	prometheusRule := &promv1.PrometheusRule{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      Name,
		Namespace: Namespace,
	}, prometheusRule); err != nil && k8serrors.IsNotFound(err) {
		logger.Info("Prometheus Rules not found. Creating...")
		prometheusRule = r.DeployPrometheusRules()
		if err := r.Client.Create(ctx, prometheusRule); err != nil {
			logger.Error(err, "Could not create Prometheus Rules")
			return ctrl.Result{Requeue: true}, fmt.Errorf("could not create Prometheus Rules: %v", err)
		}
	}

	// Deploy Operand
	cj := &batchv1.CronJob{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      Name,
		Namespace: Namespace,
	}, cj); err != nil && k8serrors.IsNotFound(err) {
		logger.Info("CronJob not found. Creating...")
		cj = r.DeployCronJob()
		if err := r.Client.Create(ctx, cj); err != nil {
			logger.Error(err, "Could not create CronJob")
			return ctrl.Result{Requeue: true}, fmt.Errorf("could not create CronJob: %v", err)
		}

		// CronJob created successfully
		// We will requeue the request to ensure the CronJob is created
		// return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AddonReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// gv, err := schema.ParseGroupVersion("starburst.group.com/v1")
	// if err != nil {
	//     return nil, err
	// }
	// newObj := unstructured.Unstructured{}
	// newObj.SetGroupVersionKind(gv.WithKind("StarburstAddon"))
	// return ctrl.NewControllerManagedBy(mgr).
	//     For(&newObj)...
	//    ... ....

	return ctrl.NewControllerManagedBy(mgr).
		For(&addonv1alpha1.Addon{}).
		Owns(&promv1.ServiceMonitor{}).
		Owns(&promv1.Prometheus{}).

		// get mounted into the cronjob
		// Used in Prometheus & ServiceMonitor
		Owns(&corev1.Secret{}).

		// apply bundle & operand
		// job instead of cronjob
		Owns(&batchv1.CronJob{}).
		Complete(r)
}

func (r *AddonReconciler) DeployCronJob() *batchv1.CronJob {
	defaultMode := int32(0755)
	failLimit := int32(3)
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name,
			Namespace: Namespace,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:               "*/1 * * * *",
			FailedJobsHistoryLimit: &failLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							ServiceAccountName: "addon-operator-controller-manager",
							Volumes: []corev1.Volume{
								{
									Name: "user-params",
									VolumeSource: corev1.VolumeSource{
										Secret: &corev1.SecretVolumeSource{
											SecretName:  "addon-managed-starburst-parameters",
											DefaultMode: &defaultMode,
										},
									},
								},
							},

							Containers: []corev1.Container{
								{
									Name:  "addon",
									Image: "cmwylie19/kube-argo-base",
									Command: []string{
										"sh",
										"-c",
										"kubectl apply -f /opt/scripts/starburstenterprise.yaml",
									},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "user-params",
											MountPath: "/opt/scripts",
											ReadOnly:  true,
										},
									},
								},
							},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}
}

func (r *AddonReconciler) DeployServiceMonitor() *promv1.ServiceMonitor {
	return &promv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name,
			Namespace: Namespace,
		},
		Spec: promv1.ServiceMonitorSpec{
			NamespaceSelector: promv1.NamespaceSelector{
				MatchNames: []string{Namespace},
			},
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "starburst-enterprise",
				},
			},
			Endpoints: []promv1.Endpoint{
				{
					Port:     "metrics",
					Interval: "2s",
				},
			},
		},
	}
}

func (r *AddonReconciler) DeployPrometheusRules() *promv1.PrometheusRule {
	return &promv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name,
			Namespace: Namespace,
			Labels: map[string]string{
				"app": "starburst",
			},
		},
		Spec: promv1.PrometheusRuleSpec{
			Groups: []promv1.RuleGroup{
				{
					Name: "starburst_alert_rules",
					Rules: []promv1.Rule{
						{
							Alert: "high_starburst_query_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("starburst_query_mem >= 45158388108"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "High Query Memory",
								"severity":    "page",
								"description": "High average memory used by all queries over a given time period",
							},
						},
						{
							Alert: "high_starburst_heap_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("starburst_heap_mem >= 45631505600"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "High Max Heap Memory",
								"severity":    "warn",
								"description": "The max amount of heap memory configured in the JVM aggregated across the entire cluster",
							},
						},
						{
							Alert: "high_starburst_max_query_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("starburst_max_query_mem >= 94489280512"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "High Heap Memory",
								"severity":    "warn",
								"description": "High amount of heap memory used by the JVMs across all cluster nodes",
							},
						},
						{
							Alert: "trino_node_failure",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("trino_active_nodes <= 1"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "Trino node failure",
								"severity":    "page",
								"description": "An active trino node went down",
							},
						},
						{
							Alert: "high_starburst_max_heap_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("starburst_max_heap_mem >= 94489280512"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "High Max Heap Memory Alert",
								"severity":    "acknowledged",
								"description": "The max amount of heap memory configured in the JVM aggregated across the entire cluster",
							},
						},
						{
							Alert: "starburst_instance_down",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("count(up{endpoint=\"metrics\"}) != 3"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "Starburst instance down",
								"severity":    "page",
								"description": "The pods churned",
							},
						},
						{
							Alert: "high_thread_count",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("sum(thread_count) > 400"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "High Thread Count",
								"severity":    "page",
								"description": "High Thread Count",
							},
						},
						{
							Alert: "JvmMemoryFillingUp",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("(sum by (instance)(jvm_memory_bytes_used{area=\"heap\"}) / sum by (instance)(jvm_memory_bytes_max{area=\"heap\"})) * 100 > 80"),
							For:  "2m",
							Annotations: map[string]string{
								"summary":     "JVM memory filling up (instance {{ $labels.instance }})",
								"severity":    "page",
								"description": "JVM memory is filling up (> 80%)\n  VALUE = {{ $value }}\n  LABELS = {{ $labels }}",
							},
						},
						{
							Alert: "starburst_failed_queries",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("failed_queries >= 4"),
							For:  "5m",
							Annotations: map[string]string{
								"summary":     "Queries are failing",
								"severity":    "page",
								"description": "In the last 5 mins the failed queries have risen",
							},
						},
					},
				},
				{
					Name: "starburst_custom_rules",
					Rules: []promv1.Rule{
						{
							Record: "starburst_query_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("avg_over_time(jvm_memory_bytes_used{endpoint=\"metrics\"}[5m])"),
						},
						{
							Record: "starburst_max_query_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("jvm_memory_bytes_max{endpoint=\"metrics\", area=\"heap\"}"),
						},
						{
							Record: "starburst_heap_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("jvm_memory_bytes_used{endpoint=\"metrics\",area=\"heap\"}"),
						},
						{
							Record: "starburst_max_heap_mem",
							//Expr:  intstr.FromString("up{job=\"starburst-enterprise\"} == 0"),
							Expr: intstr.FromString("jvm_memory_bytes_max{endpoint=\"metrics\",area=\"heap\"}"),
						},
					},
				},
			},
		},
	}
}

func (r *AddonReconciler) DeployPrometheus(tokenURL, remoteWriteURL, clusterID string) *promv1.Prometheus {
	return &promv1.Prometheus{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name,
			Namespace: Namespace,
		},
		Spec: promv1.PrometheusSpec{
			RuleSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "starburst",
				},
			},
			CommonPrometheusFields: promv1.CommonPrometheusFields{
				ExternalLabels: map[string]string{
					"cluster_id": clusterID,
				},
				LogLevel: "debug",
				RemoteWrite: []promv1.RemoteWriteSpec{
					{
						WriteRelabelConfigs: []promv1.RelabelConfig{
							{
								Action: "keep",
								Regex:  "csv_succeeded$|csv_abnormal$|cluster_version$|ALERTS$|subscription_sync_total|trino_.*$|jvm_heap_memory_used$|node_.*$|namespace_.*$|kube_.*$|cluster.*$|container_.*$",
							},
						},
						URL: remoteWriteURL,
						TLSConfig: &promv1.TLSConfig{
							SafeTLSConfig: promv1.SafeTLSConfig{
								InsecureSkipVerify: true,
							},
						},
						OAuth2: &promv1.OAuth2{
							ClientID: promv1.SecretOrConfigMap{
								Secret: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "addon",
									},
									Key: "client-id",
								},
							},
							ClientSecret: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "addon",
								},
								Key: "client-secret",
							},
							TokenURL: tokenURL,
						},
					},
				},
				ServiceMonitorNamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": "redhat-starburst-operator",
					},
				},

				ServiceMonitorSelector: &metav1.LabelSelector{},
				PodMonitorSelector:     &metav1.LabelSelector{},
				ServiceAccountName:     "starburst-enterprise-helm-operator-controller-manager",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("400Mi"),
					},
				},
			},
		},
	}
}

// returns the license secret
func (r *AddonReconciler) DeployLicenseSecret(ctx context.Context, license, namespace string) *corev1.Secret {
	logger := log.FromContext(ctx)
	logger.Info("Deploying license secret")
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "starburst-license",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"starburstdata.license": []byte(license),
		},
	}
}

func (r *AddonReconciler) DeployFederationServiceMonitor() *promv1.ServiceMonitor {
	metrics := make(map[string][]string)
	str1 := fmt.Sprintf("container_memory_working_set_bytes{namespace=\"%s\"}", Namespace)
	str2 := fmt.Sprintf("node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate{namespace=\"%s\"}", Namespace)
	str3 := fmt.Sprintf("namespace_workload_pod:kube_pod_owner:relabel{namespace=\"%s\"}", Namespace)
	str4 := fmt.Sprintf("kube_pod_container_info{namespace=\"%s\"}", Namespace)
	str5 := fmt.Sprintf("kube_pod_status_ready{namespace=\"%s\"}", Namespace)
	str6 := fmt.Sprintf("kube_pod_container_status_last_terminated_reason{namespace=\"%s\"}", Namespace)
	str7 := fmt.Sprintf("kube_pod_container_status_waiting{namespace=\"%s\"}", Namespace)
	str8 := fmt.Sprintf("kube_namespace_status_phase{namespace=\"%s\"}", Namespace)
	str9 := fmt.Sprintf("node_namespace_pod:kube_pod_info:{namespace=\"%s\"}", Namespace)
	str10 := fmt.Sprintf("kube_service_info{namespace=\"%s\"}", Namespace)
	str11 := fmt.Sprintf("cluster:namespace:pod_memory:active:kube_pod_container_resource_limits{namespace=\"%s\"}", Namespace)
	str12 := fmt.Sprintf("container_cpu_cfs_throttled_seconds_total{namespace=\"%s\"}", Namespace)
	str13 := fmt.Sprintf("container_fs_usage_bytes{namespace=\"%s\"}", Namespace)
	str14 := fmt.Sprintf("container_network_receive_bytes_total{namespace=\"%s\"}", Namespace)
	str15 := fmt.Sprintf("container_network_transmit_bytes_total{namespace=\"%s\"}", Namespace)
	str16 := fmt.Sprintf("kube_deployment_status_replicas_available{namespace=\"%s\"}", Namespace)
	str17 := "kube_node_status_capacity"
	str18 := fmt.Sprintf("container_memory_usage_bytes{namespace=\"%s\"}", Namespace)
	str19 := fmt.Sprintf("kube_pod_container_resource_requests{namespace=\"%s\"}", Namespace)
	str20 := fmt.Sprintf("kube_deployment_status_replicas_unavailable{namespace=\"%s\"}", Namespace)
	str21 := fmt.Sprintf("kube_persistentvolumeclaim_status_phase{namespace=\"%s\"}", Namespace)
	str22 := fmt.Sprintf("container_memory_working_set_bytes{namespace=\"%s\"}", Namespace)
	str23 := fmt.Sprintf("kube_pod_container_resource_limits{namespace=\"%s\"}", Namespace)
	str24 := fmt.Sprintf("cluster:namespace:pod_cpu:active:kube_pod_container_resource_limits{namespace=\"%s\"}", Namespace)
	str25 := fmt.Sprintf("container_network_receive_packets_total{namespace=\"%s\"}", Namespace)
	str26 := fmt.Sprintf("container_network_transmit_packets_total{namespace=\"%s\"}", Namespace)
	str27 := fmt.Sprintf("kube_running_pod_ready{namespace=\"%s\"}", Namespace)
	str28 := fmt.Sprintf("node_namespace_pod:kube_pod_info:{namespace=\"%s\"}", Namespace)
	str29 := fmt.Sprintf("container_cpu_usage_seconds_total{namespace=\"%s\"}", Namespace)
	str30 := fmt.Sprintf("kube_pod_container_status_restarts_total{namespace=\"%s\"}", Namespace)
	str31 := fmt.Sprintf("kube_pod_status_phase{namespace=\"%s\"}", Namespace)
	str32 := fmt.Sprintf("cluster:namespace:pod_memory:active:kube_pod_container_resource_requests{namespace=\"%s\"}", Namespace)

	metrics["match[]"] = append(metrics["match[]"], str1, str2, str3, str4, str5, str6, str7, str8, str9, str10, str11, str12, str13, str14, str15, str16, str17, str18, str19, str20, str21, str22, str23, str24, str25, str26, str27, str28, str29, str30, str31, str32)

	return &promv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name + "-federation",
			Namespace: Namespace,
		},
		Spec: promv1.ServiceMonitorSpec{
			JobLabel: "openshift-monitoring-federation",
			NamespaceSelector: promv1.NamespaceSelector{
				MatchNames: []string{
					"openshift-monitoring",
				},
			},
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance": "k8s",
				},
			},
			Endpoints: []promv1.Endpoint{
				{
					BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
					Port:            "web",
					Path:            "/federate",
					Interval:        "30s",
					Scheme:          "https",
					Params:          metrics,
					TLSConfig: &promv1.TLSConfig{
						SafeTLSConfig: promv1.SafeTLSConfig{
							InsecureSkipVerify: true,
							ServerName:         "prometheus-k8s.openshift-monitoring.svc.cluster.local",
						},
						CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt",
					},
				},
			},
		},
	}
}

func fetchClusterID() string {
	clusterID := cv.Spec.ClusterID
	return string(clusterID)
}
