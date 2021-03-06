package config

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	mf "github.com/jcrossley3/manifestival"
	sec "github.com/openshift/client-go/security/clientset/versioned/typed/security/v1"
	"github.com/operator-framework/operator-sdk/pkg/predicate"
	"github.com/prometheus/common/log"
	op "github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ClusterCRName   = "cluster"
	DefaultTargetNs = "openshift-pipelines"

	// Name of the pipeline controller deployment
	PipelineControllerName = "tekton-pipelines-controller"
	PipelineControllerSA   = "tekton-pipelines-controller"

	// Name of the pipeline webhook deployment
	PipelineWebhookName = "tekton-pipelines-webhook"
	sccAnnotationKey    = "operator.tekton.dev"
)

var (
	tektonVersion   = "v0.7.0"
	resourceWatched string
	resourceDir     string
	targetNamespace string
	noAutoInstall   bool
	recursive       bool
	ctrlLog         = logf.Log.WithName("ctrl").WithName("config")
)

func init() {
	flag.StringVar(
		&resourceWatched, "watch-resource", ClusterCRName,
		"cluster-wide resource that operator honours, default: "+ClusterCRName)

	flag.StringVar(
		&targetNamespace, "target-namespace", DefaultTargetNs,
		"Namespace where pipeline will be installed default: "+DefaultTargetNs)

	defaultResDir := filepath.Join("deploy", "resources", tektonVersion)
	flag.StringVar(
		&resourceDir, "resource-dir", defaultResDir,
		"Path to resource manifests, default: "+defaultResDir)

	flag.BoolVar(
		&noAutoInstall, "no-auto-install", false,
		"Do not automatically install tekton pipelines, default: false")

	flag.BoolVar(
		&recursive, "recursive", false,
		"If enabled apply manifest file in resource directory recursively")

	ctrlLog.Info("configuration",
		"resource-watched", resourceWatched,
		"targetNamespace", targetNamespace,
		"no-auto-install", noAutoInstall,
	)
}

// Add creates a new Config Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	m, err := mf.NewManifest(resourceDir, recursive, mgr.GetClient())
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, m))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, m mf.Manifest) reconcile.Reconciler {
	secClient, _ := sec.NewForConfig(mgr.GetConfig())
	return &ReconcileConfig{
		client:    mgr.GetClient(),
		scheme:    mgr.GetScheme(),
		secClient: secClient,
		manifest:  m,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	log := ctrlLog.WithName("add")
	// Create a new controller
	c, err := controller.New("config-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Config
	log.Info("Watching operator config CR")
	err = c.Watch(
		&source.Kind{Type: &op.Config{}},
		&handler.EnqueueRequestForObject{},
		predicate.GenerationChangedPredicate{},
	)
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &appsv1.Deployment{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &op.Config{},
		})
	if err != nil {
		return err
	}

	if noAutoInstall {
		return nil
	}

	if err := createCR(mgr.GetClient()); err != nil {
		log.Error(err, "creation of config resource failed")
		return err
	}
	return nil
}

// blank assignment to verify that ReconcileConfig implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileConfig{}

// ReconcileConfig reconciles a Config object
type ReconcileConfig struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	secClient *sec.SecurityV1Client
	scheme    *runtime.Scheme
	manifest  mf.Manifest
}

// Reconcile reads that state of the cluster for a Config object and makes changes based on the state read
// and what is in the Config.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileConfig) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	log := requestLogger(req, "reconcile")

	log.Info("reconciling config change")

	cfg := &op.Config{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: req.Name}, cfg)

	// ignore all resources except the `resourceWatched`
	if req.Name != resourceWatched {
		log.Info("ignoring incorrect object")

		// handle resources that are not interesting as error
		if !errors.IsNotFound(err) {
			r.markInvalidResource(cfg)
		}
		return reconcile.Result{}, nil
	}

	// handle deletion of resource
	if errors.IsNotFound(err) {
		// User deleted the cluster resource so delete the pipeine resources
		log.Info("resource has been deleted", "config", cfg.Spec, "status", cfg.Status)
		return r.reconcileDeletion(req, cfg)
	}

	// Error reading the object - requeue the request.
	if err != nil {
		log.Error(err, "requeueing event since there was an error reading object")
		return reconcile.Result{}, err
	}

	if isUpToDate(cfg) {
		log.Info("skipping installation, resource already up to date")
		return reconcile.Result{}, nil
	}

	log.Info("installing pipelines", "path", resourceDir)

	return r.reconcileInstall(req, cfg)

}

func (r *ReconcileConfig) reconcileInstall(req reconcile.Request, cfg *op.Config) (reconcile.Result, error) {
	log := requestLogger(req, "install")

	err := r.updateStatus(cfg, op.ConfigCondition{Code: op.InstallingStatus, Version: tektonVersion})
	if err != nil {
		log.Error(err, "failed to set status")
		return reconcile.Result{}, err
	}

	tfs := []mf.Transformer{
		mf.InjectOwner(cfg),
		mf.InjectNamespace(cfg.Spec.TargetNamespace),
	}

	if err := r.manifest.Transform(tfs...); err != nil {
		log.Error(err, "failed to apply manifest transformations")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	if err := r.manifest.ApplyAll(); err != nil {
		log.Error(err, "failed to apply release.yaml")
		// ignoring failure to update
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}
	log.Info("successfully applied all resources")

	// NOTE: manifest when updating (not installing) already installed resources
	// modifies the `cfg` but does not refersh it, hence refresh manually
	if err := r.refreshCR(cfg); err != nil {
		log.Error(err, "status update failed to refresh object")
		return reconcile.Result{}, err
	}

	// add pipeline-controller to scc; scc privileged needs to be updated and
	// can't be just oc applied
	controller := types.NamespacedName{Namespace: cfg.Spec.TargetNamespace, Name: PipelineControllerName}
	ctrlSA, err := r.serviceAccountNameForDeployment(controller)
	if err != nil {
		log.Error(err, "failed to find controller service account")
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	if err := r.addPrivilegedSCC(ctrlSA); err != nil {
		log.Error(err, "failed to update scc")
		_ = r.updateStatus(cfg, op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: err.Error(),
			Version: tektonVersion})
		return reconcile.Result{}, err
	}

	err = r.updateStatus(cfg, op.ConfigCondition{Code: op.InstalledStatus, Version: tektonVersion})
	return reconcile.Result{}, err
}

func (r *ReconcileConfig) serviceAccountNameForDeployment(deployment types.NamespacedName) (string, error) {
	d := appsv1.Deployment{}
	if err := r.client.Get(context.Background(), deployment, &d); err != nil {
		return "", err
	}

	sa := d.Spec.Template.Spec.ServiceAccountName
	fullSA := fmt.Sprintf("system:serviceaccount:%s:%s", deployment.Namespace, sa)
	return fullSA, nil

}

func (r *ReconcileConfig) addPrivilegedSCC(sa string) error {
	log := ctrlLog.WithName("scc").WithName("add")
	privileged, err := r.secClient.SecurityContextConstraints().Get("privileged", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "scc privileged get error")
		return err
	}

	newList, changed := addToList(privileged.Users, sa)
	_, annotated := privileged.Annotations[sccAnnotationKey]
	if !changed && annotated {
		log.Info("scc already in added to the list", "action", "none")
		return nil
	}

	log.Info("privileged scc needs updation")
	privileged.Annotations[sccAnnotationKey] = sa
	privileged.Users = newList

	updated, err := r.secClient.SecurityContextConstraints().Update(privileged)
	log.Info("added SA to scc", "updated", updated.Users)
	return err
}

func (r *ReconcileConfig) removePrivilegedSCC() error {
	log := ctrlLog.WithName("scc").WithName("remove")
	privileged, err := r.secClient.SecurityContextConstraints().Get("privileged", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "scc privileged get error")
		return err
	}

	sa, annotated := privileged.Annotations[sccAnnotationKey]
	if !annotated {
		log.Info("sa already not in privileged SCC", "action", "none")
		return nil
	}

	newList, changed := removeFromList(privileged.Users, sa)
	if !changed {
		log.Info("sa already not in privileged SCC", "action", "none")
		return nil
	}

	log.Info("privileged scc needs updation")
	delete(privileged.Annotations, sccAnnotationKey)
	privileged.Users = newList

	updated, err := r.secClient.SecurityContextConstraints().Update(privileged)
	log.Info("removed SA from scc", "updated", updated.Users)
	return err
}

func removeFromList(list []string, item string) ([]string, bool) {
	for i, v := range list {
		if v == item {
			return append(list[:i], list[i+1:]...), true
		}
	}
	return list, false
}

func addToList(list []string, item string) ([]string, bool) {
	for _, v := range list {
		if v == item {
			return list, false
		}
	}
	return append(list, item), true
}

func (r *ReconcileConfig) reconcileDeletion(req reconcile.Request, cfg *op.Config) (reconcile.Result, error) {
	log := requestLogger(req, "delete")

	log.Info("deleting pipeline resources")

	if err := r.removePrivilegedSCC(); err != nil {
		return reconcile.Result{}, err
	}

	// Requested object not found, could have been deleted after reconcile request.
	// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
	propPolicy := client.PropagationPolicy(metav1.DeletePropagationForeground)

	if err := r.manifest.DeleteAll(propPolicy); err != nil {
		log.Error(err, "failed to delete pipeline resources")
		return reconcile.Result{}, err
	}

	// Return and don't requeue
	return reconcile.Result{}, nil
}

// markInvalidResource sets the status of resourse as invalid
func (r *ReconcileConfig) markInvalidResource(cfg *op.Config) {
	err := r.updateStatus(cfg,
		op.ConfigCondition{
			Code:    op.ErrorStatus,
			Details: "metadata.name must be " + resourceWatched,
			Version: "unknown"})
	if err != nil {
		ctrlLog.Info("failed to update status as invalid")
	}
}

// updateStatus set the status of cfg to s and refreshes cfg to the lastest version
func (r *ReconcileConfig) updateStatus(cfg *op.Config, c op.ConfigCondition) error {

	// NOTE: need to use a deepcopy since Status().Update() seems to reset the
	// APIVersion of the cfg to "" making the object invalid; may be a mechanism
	// to prevent us from using stale version of the object

	tmp := cfg.DeepCopy()
	tmp.Status.Conditions = append([]op.ConfigCondition{c}, tmp.Status.Conditions...)

	if err := r.client.Status().Update(context.TODO(), tmp); err != nil {
		log.Error(err, "status update failed")
		return err
	}

	if err := r.refreshCR(cfg); err != nil {
		log.Error(err, "status update failed to refresh object")
		return err
	}
	return nil
}

func (r *ReconcileConfig) refreshCR(cfg *op.Config) error {
	objKey := types.NamespacedName{
		Namespace: cfg.Namespace,
		Name:      cfg.Name,
	}
	return r.client.Get(context.TODO(), objKey, cfg)
}

func createCR(c client.Client) error {
	log := ctrlLog.WithName("create-cr").WithValues("name", resourceWatched)
	log.Info("creating a clusterwide resource of config crd")

	cr := &op.Config{
		ObjectMeta: metav1.ObjectMeta{Name: resourceWatched},
		Spec:       op.ConfigSpec{TargetNamespace: targetNamespace},
	}

	err := c.Create(context.TODO(), cr)
	if errors.IsAlreadyExists(err) {
		log.Info("skipped creation", "reason", "resoure already exists")
		return nil
	}

	return err
}

func isUpToDate(r *op.Config) bool {
	c := r.Status.Conditions
	if len(c) == 0 {
		return false
	}

	latest := c[0]
	return latest.Version == tektonVersion &&
		latest.Code == op.InstalledStatus
}

func requestLogger(req reconcile.Request, context string) logr.Logger {
	return ctrlLog.WithName(context).WithValues(
		"Request.Namespace", req.Namespace,
		"Request.NamespaceName", req.NamespacedName,
		"Request.Name", req.Name)
}
