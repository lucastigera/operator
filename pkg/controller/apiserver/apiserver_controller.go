// Copyright (c) 2020-2025 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiserver

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"

	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v3 "github.com/tigera/api/pkg/apis/projectcalico/v3"

	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/common/validation"
	apiserver "github.com/tigera/operator/pkg/common/validation/apiserver"
	"github.com/tigera/operator/pkg/controller/certificatemanager"
	"github.com/tigera/operator/pkg/controller/k8sapi"
	"github.com/tigera/operator/pkg/controller/options"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/controller/utils/imageset"
	"github.com/tigera/operator/pkg/ctrlruntime"
	"github.com/tigera/operator/pkg/dns"
	"github.com/tigera/operator/pkg/render"
	rcertificatemanagement "github.com/tigera/operator/pkg/render/certificatemanagement"
	"github.com/tigera/operator/pkg/render/common/authentication"
	"github.com/tigera/operator/pkg/render/common/networkpolicy"
	"github.com/tigera/operator/pkg/render/monitor"
	"github.com/tigera/operator/pkg/tls/certificatemanagement"
)

const ResourceName string = "apiserver"

var log = logf.Log.WithName("controller_apiserver")

// Add creates a new APIServer Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, opts options.AddOptions) error {
	r := newReconciler(mgr, opts)

	c, err := ctrlruntime.NewController("apiserver-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return fmt.Errorf("failed to create apiserver-controller: %w", err)
	}

	// Established deferred watches against the v3 API that should succeed after the Enterprise API Server becomes available.
	if opts.EnterpriseCRDExists {
		// Watch for changes to Tier, as its status is used as input to determine whether network policy should be reconciled by this controller.
		go utils.WaitToAddTierWatch(networkpolicy.TigeraComponentTierName, c, opts.K8sClientset, log, r.tierWatchReady)

		go utils.WaitToAddNetworkPolicyWatches(c, opts.K8sClientset, log, []types.NamespacedName{
			{Name: render.APIServerPolicyName, Namespace: render.APIServerNamespace},
		})
	}

	return add(c, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, opts options.AddOptions) *ReconcileAPIServer {
	r := &ReconcileAPIServer{
		client:              mgr.GetClient(),
		scheme:              mgr.GetScheme(),
		provider:            opts.DetectedProvider,
		enterpriseCRDsExist: opts.EnterpriseCRDExists,
		status:              status.New(mgr.GetClient(), "apiserver", opts.KubernetesVersion),
		clusterDomain:       opts.ClusterDomain,
		tierWatchReady:      &utils.ReadyFlag{},
		multiTenant:         opts.MultiTenant,
		kubernetesVersion:   opts.KubernetesVersion,
	}
	r.status.Run(opts.ShutdownContext)
	return r
}

// add adds watches for resources that are available at startup
func add(c ctrlruntime.Controller, r *ReconcileAPIServer) error {
	// Watch for changes to primary resource APIServer
	err := c.WatchObject(&operatorv1.APIServer{}, &handler.EnqueueRequestForObject{})
	if err != nil {
		log.V(5).Info("Failed to create APIServer watch", "err", err)
		return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
	}

	if err = utils.AddInstallationWatch(c); err != nil {
		log.V(5).Info("Failed to create network watch", "err", err)
		return fmt.Errorf("apiserver-controller failed to watch Tigera network resource: %v", err)
	}

	if err = utils.AddConfigMapWatch(c, render.K8sSvcEndpointConfigMapName, common.OperatorNamespace(), &handler.EnqueueRequestForObject{}); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch ConfigMap %s: %w", render.K8sSvcEndpointConfigMapName, err)
	}

	if r.enterpriseCRDsExist {
		// Watch for changes to ApplicationLayer
		err = c.WatchObject(&operatorv1.ApplicationLayer{ObjectMeta: metav1.ObjectMeta{Name: utils.DefaultTSEEInstanceKey.Name}}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch ApplicationLayer resource: %v", err)
		}

		// Watch for changes to primary resource ManagementCluster
		err = c.WatchObject(&operatorv1.ManagementCluster{}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
		}

		// Watch for changes to primary resource ManagementClusterConnection
		err = c.WatchObject(&operatorv1.ManagementClusterConnection{}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch primary resource: %v", err)
		}

		for _, namespace := range []string{common.OperatorNamespace(), render.APIServerNamespace} {
			for _, secretName := range []string{render.VoltronTunnelSecretName, render.ManagerTLSSecretName} {
				if err = utils.AddSecretsWatch(c, secretName, namespace); err != nil {
					return fmt.Errorf("apiserver-controller failed to watch the Secret resource: %v", err)
				}
			}
		}

		// Watch for changes to authentication
		err = c.WatchObject(&operatorv1.Authentication{}, &handler.EnqueueRequestForObject{})
		if err != nil {
			return fmt.Errorf("apiserver-controller failed to watch resource: %w", err)
		}

	}

	// Watch for the namespace(s) managed by this controller.
	if err = c.WatchObject(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: render.APIServerNamespace}}, &handler.EnqueueRequestForObject{}); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch resource: %w", err)
	}

	for _, secretName := range []string{
		"calico-apiserver-certs",
		certificatemanagement.CASecretName,
		render.DexTLSSecretName,
		monitor.PrometheusClientTLSSecretName,
	} {
		if err = utils.AddSecretsWatch(c, secretName, common.OperatorNamespace()); err != nil {
			return fmt.Errorf("apiserver-controller failed to watch the Secret resource: %v", err)
		}
	}

	// Watch for changes to objects created by this controller.
	if err = utils.AddDeploymentWatch(c, "tigera-apiserver", "calico-system"); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch Deployment: %w", err)
	}
	if err = utils.AddDeploymentWatch(c, "calico-apiserver", "calico-apiserver"); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch Deployment: %w", err)
	}

	if err = imageset.AddImageSetWatch(c); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch ImageSet: %w", err)
	}

	// Watch for changes to TigeraStatus.
	if err = utils.AddTigeraStatusWatch(c, ResourceName); err != nil {
		return fmt.Errorf("apiserver-controller failed to watch apiserver Tigerastatus: %w", err)
	}

	// Perform periodic reconciliation. This acts as a backstop to catch reconcile issues,
	// and also makes sure we spot when things change that might not trigger a reconciliation.
	if err = utils.AddPeriodicReconcile(c, utils.PeriodicReconcileTime, &handler.EnqueueRequestForObject{}); err != nil {
		return fmt.Errorf("apiserver-controller failed to create periodic reconcile watch: %w", err)
	}

	log.V(5).Info("Controller created and Watches setup")
	return nil
}

// blank assignment to verify that ReconcileAPIServer implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileAPIServer{}

// ReconcileAPIServer reconciles a APIServer object
type ReconcileAPIServer struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client              client.Client
	scheme              *runtime.Scheme
	provider            operatorv1.Provider
	enterpriseCRDsExist bool
	status              status.StatusManager
	clusterDomain       string
	tierWatchReady      *utils.ReadyFlag
	multiTenant         bool
	kubernetesVersion   *common.VersionInfo
}

// Reconcile reads that state of the cluster for a APIServer object and makes changes based on the state read
// and what is in the APIServer.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileAPIServer) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.V(2).Info("Reconciling APIServer")

	instance, msg, err := utils.GetAPIServer(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("APIServer config not found")
			r.status.OnCRNotFound()
			return reconcile.Result{}, r.maintainFinalizer(ctx, nil)
		}
		r.status.SetDegraded(operatorv1.ResourceReadError, fmt.Sprintf("An error occurred when querying the APIServer resource: %s", msg), err, reqLogger)
		return reconcile.Result{}, err
	}
	r.status.OnCRFound()
	reqLogger.V(2).Info("Loaded config", "config", instance)

	// Validate APIServer resource.
	if err := validateAPIServerResource(instance); err != nil {
		r.status.SetDegraded(operatorv1.ResourceValidationError, "APIServer is invalid", err, reqLogger)
		return reconcile.Result{}, err
	}

	// SetMetaData in the TigeraStatus such as observedGenerations.
	defer r.status.SetMetaData(&instance.ObjectMeta)

	// Changes for updating ApiServer status conditions.
	if request.Name == ResourceName && request.Namespace == "" {
		ts := &operatorv1.TigeraStatus{}
		err := r.client.Get(ctx, types.NamespacedName{Name: ResourceName}, ts)
		if err != nil {
			return reconcile.Result{}, err
		}
		instance.Status.Conditions = status.UpdateStatusCondition(instance.Status.Conditions, ts.Status.Conditions)
		if err := r.client.Status().Update(ctx, instance); err != nil {
			log.WithValues("reason", err).Info("Failed to create apiserver status conditions.")
			return reconcile.Result{}, err
		}
	}

	// Query for the installation object.
	_, installationSpec, err := utils.GetInstallation(context.Background(), r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			r.status.SetDegraded(operatorv1.ResourceNotFound, "Installation not found", err, reqLogger)
			return reconcile.Result{}, err
		}
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error querying installation", err, reqLogger)
		return reconcile.Result{}, err
	}
	if installationSpec.Variant == "" {
		r.status.SetDegraded(operatorv1.ResourceNotReady, "Waiting for Installation Variant to be set", nil, reqLogger)
		return reconcile.Result{}, nil
	}

	certificateManager, err := certificatemanager.Create(r.client, installationSpec, r.clusterDomain, common.OperatorNamespace())
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceCreateError, "Unable to create the Tigera CA", err, reqLogger)
		return reconcile.Result{}, err
	}

	secretName := render.CalicoAPIServerTLSSecretName
	tlsSecret, err := certificateManager.GetOrCreateKeyPair(r.client, secretName, common.OperatorNamespace(), dns.GetServiceDNSNames(render.APIServerServiceName, render.APIServerNamespace, r.clusterDomain))
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceCreateError, "Unable to get or create tls key pair", err, reqLogger)
		return reconcile.Result{}, err
	}

	// Since apiserver and queryserver may have different UID:GID at run-time, we need to produce this secret in separate volumes and with different permissions.
	var queryServerTLSSecretCertificateManagementOnly certificatemanagement.KeyPairInterface
	if installationSpec.CertificateManagement != nil {
		queryServerTLSSecretCertificateManagementOnly, err = certificateManager.GetOrCreateKeyPair(r.client, "query-server-tls", common.OperatorNamespace(), dns.GetServiceDNSNames(render.APIServerServiceName, render.APIServerNamespace, r.clusterDomain))
		if err != nil {
			r.status.SetDegraded(operatorv1.ResourceCreateError, "Unable to get or create tls key pair", err, reqLogger)
			return reconcile.Result{}, err
		}
	}

	certificateManager.AddToStatusManager(r.status, render.APIServerNamespace)

	pullSecrets, err := utils.GetNetworkingPullSecrets(installationSpec, r.client)
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error retrieving pull secrets", err, reqLogger)
		return reconcile.Result{}, err
	}

	// Query enterprise-only data.
	var tunnelCAKeyPair certificatemanagement.KeyPairInterface
	var trustedBundle certificatemanagement.TrustedBundle
	var applicationLayer *operatorv1.ApplicationLayer
	var managementCluster *operatorv1.ManagementCluster
	var managementClusterConnection *operatorv1.ManagementClusterConnection
	var keyValidatorConfig authentication.KeyValidatorConfig
	includeV3NetworkPolicy := false

	if installationSpec.Variant == operatorv1.TigeraSecureEnterprise {
		trustedBundle, err = certificateManager.CreateNamedTrustedBundleFromSecrets(render.APIServerResourceName, r.client,
			common.OperatorNamespace(), false)
		if err != nil {
			r.status.SetDegraded(operatorv1.ResourceCreateError, "Unable to create the trusted bundle", err, reqLogger)
			return reconcile.Result{}, err
		}

		applicationLayer, err = utils.GetApplicationLayer(ctx, r.client)
		if err != nil {
			r.status.SetDegraded(operatorv1.ResourceReadError, "Error reading ApplicationLayer", err, reqLogger)
			return reconcile.Result{}, err
		}

		managementCluster, err = utils.GetManagementCluster(ctx, r.client)
		if err != nil {
			r.status.SetDegraded(operatorv1.ResourceReadError, "Error reading ManagementCluster", err, reqLogger)
			return reconcile.Result{}, err
		}

		managementClusterConnection, err = utils.GetManagementClusterConnection(ctx, r.client)
		if err != nil {
			r.status.SetDegraded(operatorv1.ResourceReadError, "Error reading ManagementClusterConnection", err, reqLogger)
			return reconcile.Result{}, err
		}

		if managementClusterConnection != nil && managementCluster != nil {
			err = fmt.Errorf("having both a ManagementCluster and a ManagementClusterConnection is not supported")
			r.status.SetDegraded(operatorv1.ResourceValidationError, "", err, reqLogger)
			return reconcile.Result{}, err
		}

		// This block depends on the Manager controller having defaulted the ManagementCluster CR and having created the tunnel CA secret.
		// If these conditions are not met, this controller does not degrade as the Manager controller needs API server to be ready to accomplish the above.
		if managementCluster != nil && managementCluster.Spec.TLS != nil && !r.multiTenant {
			// The secret that contains the CA x509 certificate to create client certificates for the managed cluster
			// is created by the Manager controller in tigera-operator namespace. We will read this secret and make
			// sure it is available in the same namespace as the API server (calico-system)
			// This secret is only created for a management cluster in a multi-cluster setup for a single tenant.
			// Other cluster types do not require this secret. (Standalone configuration do not need it and multi-tenant
			// configuration create secrets inside the tenant namespaces)
			tunnelSecretName := managementCluster.Spec.TLS.SecretName
			tunnelCASecret, err := utils.GetSecret(ctx, r.client, tunnelSecretName, common.OperatorNamespace())
			if err != nil {
				r.status.SetDegraded(operatorv1.ResourceReadError, "Unable to fetch the tunnel secret", err, reqLogger)
				return reconcile.Result{}, err
			}
			if tunnelCASecret != nil {
				tunnelCAKeyPair = certificatemanagement.NewKeyPair(tunnelCASecret, nil, "")
			}
		}

		// Ensure the allow-tigera tier exists, before rendering any network policies within it.
		//
		// The creation of the Tier depends on this controller to reconcile it's non-NetworkPolicy resources so that
		// the API Server becomes available. Therefore, if we fail to query the Tier, we exclude NetworkPolicy from
		// reconciliation and tolerate errors arising from the Tier not being created or the API server not being available.
		// We also exclude NetworkPolicy and do not degrade when the Tier watch is not ready, as this means the API server is not available.
		if r.tierWatchReady.IsReady() {
			if err := r.client.Get(ctx, client.ObjectKey{Name: networkpolicy.TigeraComponentTierName}, &v3.Tier{}); err != nil {
				if !errors.IsNotFound(err) && !meta.IsNoMatchError(err) {
					r.status.SetDegraded(operatorv1.ResourceReadError, "Error querying allow-tigera tier", err, reqLogger)
					return reconcile.Result{}, err
				}
			} else {
				includeV3NetworkPolicy = true
			}
		}

		prometheusCertificate, err := certificateManager.GetCertificate(r.client, monitor.PrometheusClientTLSSecretName, common.OperatorNamespace())
		if err != nil {
			r.status.SetDegraded(operatorv1.ResourceReadError, "Failed to get certificate", err, reqLogger)
			return reconcile.Result{}, err
		} else if prometheusCertificate != nil {
			trustedBundle.AddCertificates(prometheusCertificate)
		}

		var authenticationCR *operatorv1.Authentication
		// Fetch the Authentication spec. If present, we use it to configure user authentication.
		authenticationCR, err = utils.GetAuthentication(ctx, r.client)
		if err != nil && !errors.IsNotFound(err) {
			r.status.SetDegraded(operatorv1.ResourceReadError, "Error while fetching Authentication", err, reqLogger)
			return reconcile.Result{}, err
		}

		if authenticationCR != nil && authenticationCR.Status.State == operatorv1.TigeraStatusReady {
			if utils.DexEnabled(authenticationCR) {
				// Do not include DEX TLS Secret Name if authentication CR does not have type Dex
				secret := render.DexTLSSecretName
				certificate, err := certificateManager.GetCertificate(r.client, secret, common.OperatorNamespace())
				if err != nil {
					r.status.SetDegraded(operatorv1.CertificateError, fmt.Sprintf("Failed to retrieve %s", secret),
						err, reqLogger)
					return reconcile.Result{}, err
				} else if certificate == nil {
					reqLogger.Info(fmt.Sprintf("Waiting for secret '%s' to become available", secret))
					r.status.SetDegraded(operatorv1.ResourceNotReady,
						fmt.Sprintf("Waiting for secret '%s' to become available", secret),
						nil, reqLogger)
					return reconcile.Result{}, nil
				}
				trustedBundle.AddCertificates(certificate)
			}

			keyValidatorConfig, err = utils.GetKeyValidatorConfig(ctx, r.client, authenticationCR, r.clusterDomain)
			if err != nil {
				r.status.SetDegraded(operatorv1.ResourceReadError, "Failed to get KeyValidator Config", err, reqLogger)
				return reconcile.Result{}, err
			}
		}
	}

	err = utils.PopulateK8sServiceEndPoint(r.client)
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error reading services endpoint configmap", err, reqLogger)
		return reconcile.Result{}, err
	}

	// API server exists and configuration is valid - maintain a Finalizer on the installation.
	if err := r.maintainFinalizer(ctx, instance); err != nil {
		r.status.SetDegraded(operatorv1.ResourceReadError, "Error setting finalizer on Installation", err, reqLogger)
		return reconcile.Result{}, err
	}

	// Create a component handler to manage the rendered component.
	handler := utils.NewComponentHandler(log, r.client, r.scheme, instance)

	// Render the desired objects from the CRD and create or update them.
	reqLogger.V(3).Info("rendering components")

	apiServerCfg := render.APIServerConfiguration{
		K8SServiceEndpoint:          k8sapi.Endpoint,
		Installation:                installationSpec,
		APIServer:                   &instance.Spec,
		ForceHostNetwork:            false,
		ApplicationLayer:            applicationLayer,
		ManagementCluster:           managementCluster,
		ManagementClusterConnection: managementClusterConnection,
		TLSKeyPair:                  tlsSecret,
		PullSecrets:                 pullSecrets,
		OpenShift:                   r.provider.IsOpenShift(),
		TrustedBundle:               trustedBundle,
		MultiTenant:                 r.multiTenant,
		KeyValidatorConfig:          keyValidatorConfig,
		KubernetesVersion:           r.kubernetesVersion,
		CanCleanupOlderResources:    r.canCleanupLegacyNamespace(ctx, installationSpec.Variant, reqLogger),
		QueryServerTLSKeyPairCertificateManagementOnly: queryServerTLSSecretCertificateManagementOnly,
	}

	var components []render.Component

	// v3 NetworkPolicy will fail to reconcile if the API server deployment is unhealthy. In case the API Server
	// deployment becomes unhealthy and reconciliation of non-NetworkPolicy resources in the apiserver controller
	// would resolve it, we render the network policies of components last to prevent a chicken-and-egg scenario.
	if includeV3NetworkPolicy {
		components = append(components, render.APIServerPolicy(&apiServerCfg))
	}

	component, err := render.APIServer(&apiServerCfg)
	if err != nil {
		r.status.SetDegraded(operatorv1.ResourceRenderingError, "Error rendering APIServer", err, reqLogger)
		return reconcile.Result{}, err
	}

	components = append(components,
		component,
		rcertificatemanagement.CertificateManagement(&rcertificatemanagement.Config{
			Namespace:       render.APIServerNamespace,
			ServiceAccounts: []string{render.APIServerServiceAccountName},
			KeyPairOptions: []rcertificatemanagement.KeyPairOption{
				rcertificatemanagement.NewKeyPairOption(tlsSecret, true, true),
				rcertificatemanagement.NewKeyPairOption(tunnelCAKeyPair, false, true),
			},
			TrustedBundle: trustedBundle,
		}),
	)

	if err = imageset.ApplyImageSet(ctx, r.client, installationSpec.Variant, components...); err != nil {
		r.status.SetDegraded(operatorv1.ResourceUpdateError, "Error with images from ImageSet", err, reqLogger)
		return reconcile.Result{}, err
	}

	for _, component := range components {
		if err := handler.CreateOrUpdateOrDelete(context.Background(), component, r.status); err != nil {
			r.status.SetDegraded(operatorv1.ResourceUpdateError, "Error creating / updating resource", err, reqLogger)
			return reconcile.Result{}, err
		}
	}

	// Clear the degraded bit if we've reached this far.
	r.status.ClearDegraded()

	if !r.status.IsAvailable() {
		// Schedule a kick to check again in the near future. Hopefully by then things will be available.
		return reconcile.Result{RequeueAfter: utils.StandardRetry}, nil
	}

	// Everything is available - update the CRD status.
	instance.Status.State = operatorv1.TigeraStatusReady
	if err = r.client.Status().Update(ctx, instance); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func validateAPIServerResource(instance *operatorv1.APIServer) error {
	// Verify the APIServerDeployment overrides, if specified, is valid.
	if d := instance.Spec.APIServerDeployment; d != nil {
		err := validation.ValidateReplicatedPodResourceOverrides(d, apiserver.ValidateAPIServerDeploymentContainer, apiserver.ValidateAPIServerDeploymentInitContainer)
		if err != nil {
			return fmt.Errorf("APIServer spec.APIServerDeployment is not valid: %w", err)
		}
	}
	return nil
}

// maintainFinalizer manages this controller's finalizer on the Installation resource.
// We add a finalizer to the Installation when the API server has been installed, and only remove that finalizer when
// the API server has been deleted and its pods have stopped running. This allows for a graceful cleanup of API server resources
// prior to the CNI plugin being removed.
func (r *ReconcileAPIServer) maintainFinalizer(ctx context.Context, apiserver client.Object) error {
	// These objects require graceful termination before the CNI plugin is torn down.
	apiServerDeployment := v1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "calico-apiserver", Namespace: render.APIServerNamespace}}
	return utils.MaintainInstallationFinalizer(ctx, r.client, apiserver, render.APIServerFinalizer, &apiServerDeployment)
}

// canCleanupLegacyNamespace determines whether the legacy "tigera-system" namespace
// (which previously hosted the API server) can be safely cleaned up.
// It returns true only if:
// - The new API server deployment in "calico-system" exists and is available.
// - The old API server deployment in "tigera-system" is either removed or inactive.
// - Both the APIServer custom resource and the TigeraStatus for 'apiserver' are in the Ready state
func (r *ReconcileAPIServer) canCleanupLegacyNamespace(ctx context.Context, variant operatorv1.ProductVariant, logger logr.Logger) bool {
	newNamespace := "calico-system"
	oldNamespace := "tigera-system"
	deploymentName := "calico-apiserver"

	if variant == operatorv1.Calico {
		oldNamespace = "calico-apiserver"
	}

	// Fetch the new API server deployment in calico-system
	newDeploy := &appsv1.Deployment{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: newNamespace}, newDeploy); err != nil {
		if errors.IsNotFound(err) {
			logger.V(3).Info("New API server not found", "ns", newNamespace)
		} else {
			logger.Error(err, "Failed to get new API server", "ns", newNamespace)
		}
		return false
	}
	if newDeploy.Status.AvailableReplicas == 0 {
		logger.V(3).Info("New API server has 0 replicas", "ns", newNamespace)
		return false
	}

	// Fetch the old API server deployment in tigera-system
	oldDeploy := &appsv1.Deployment{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: oldNamespace}, oldDeploy); err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "Failed to get old API server", "ns", oldNamespace)
			return false
		}
	}
	if oldDeploy.Status.AvailableReplicas > 0 {
		logger.V(3).Info("Old API server still running", "ns", oldNamespace)
		return false
	}

	// Ensure the new API server is functionally ready
	if !utils.IsAPIServerReady(r.client, logger) {
		logger.V(3).Info("APIServer CR is not ready")
		return false
	}

	// Ensure TigeraStatus indicates readiness
	ts := &operatorv1.TigeraStatus{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: ResourceName}, ts); err != nil {
		if errors.IsNotFound(err) {
			logger.V(3).Info("TigeraStatus not found")
		} else {
			logger.Error(err, "Failed to get TigeraStatus resource.")
		}
		return false
	}
	if !ts.Available() {
		logger.V(3).Info("TigeraStatus for apiserver is not in Available status.")
		return false
	}

	logger.V(2).Info("Safe to clean up old namespace for apiserver.")
	return true
}
