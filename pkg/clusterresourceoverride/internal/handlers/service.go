package handlers

import (
	"context"

	autoscalingv1 "github.com/openshift/cluster-resource-override-admission-operator/pkg/apis/autoscaling/v1"
	"github.com/openshift/cluster-resource-override-admission-operator/pkg/apis/reference"
	"github.com/openshift/cluster-resource-override-admission-operator/pkg/asset"
	"github.com/openshift/cluster-resource-override-admission-operator/pkg/clusterresourceoverride/internal/condition"
	"github.com/openshift/cluster-resource-override-admission-operator/pkg/ensurer"
	"github.com/openshift/cluster-resource-override-admission-operator/pkg/secondarywatch"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	controllerreconciler "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// duplicated from https://github.com/openshift/service-ca-operator/blob/422ebd8b9450954626c765911238162f16beca10/pkg/controller/api/api.go#L71
	AlphaServiceNameAnnotation = "service.alpha.openshift.io/originating-service-name"
)

func NewServiceHandler(o *Options) *serviceHandler {
	return &serviceHandler{
		dynamic: ensurer.NewServiceEnsurer(o.Client.Dynamic),
		lister:  o.SecondaryLister,
		asset:   o.Asset,
		client:  o.Client.Kubernetes,
	}
}

type serviceHandler struct {
	dynamic *ensurer.ServiceEnsurer
	lister  *secondarywatch.Lister
	asset   *asset.Asset
	client  kubernetes.Interface
}

func (s *serviceHandler) Handle(ctx *ReconcileRequestContext, original *autoscalingv1.ClusterResourceOverride) (current *autoscalingv1.ClusterResourceOverride, result controllerreconciler.Result, handleErr error) {
	current = original

	name := s.asset.Service().Name()

	secretName := s.asset.ServiceServingSecret().Name()
	secret, err := s.lister.CoreV1SecretLister().Secrets(ctx.WebhookNamespace()).Get(secretName)
	if err == nil {
		// make sure the secret is not the old secret with self-signed generated cert one prior to 4.17
		value, exists := secret.Annotations[AlphaServiceNameAnnotation]
		if !exists || (exists && value != name) { // this means the secret is still old
			err = s.client.CoreV1().Secrets(ctx.WebhookNamespace()).Delete(context.TODO(), secretName, v1.DeleteOptions{})
			if err != nil {
				handleErr = condition.NewInstallReadinessError(autoscalingv1.InternalError, err)
				return
			}
		}
	} else if !k8serrors.IsNotFound(err) {
		handleErr = condition.NewInstallReadinessError(autoscalingv1.CertNotAvailable, err)
		return
	}

	desired := s.asset.Service().New()
	ctx.ControllerSetter().Set(desired, original)

	object, err := s.lister.CoreV1ServiceLister().Services(ctx.WebhookNamespace()).Get(name)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			handleErr = condition.NewInstallReadinessError(autoscalingv1.CertNotAvailable, err)
			return
		}

		service, err := s.dynamic.Ensure(desired)
		if err != nil {
			handleErr = condition.NewInstallReadinessError(autoscalingv1.CertNotAvailable, err)
			return
		}

		object = service
		klog.V(2).Infof("key=%s resource=%T/%s successfully created", original.Name, object, object.Name)
	}

	if ref := current.Status.Resources.ServiceRef; ref != nil && ref.ResourceVersion == object.ResourceVersion {
		klog.V(2).Infof("key=%s resource=%T/%s is in sync", original.Name, object, object.Name)
		return
	}

	newRef, err := reference.GetReference(object)
	if err != nil {
		handleErr = condition.NewInstallReadinessError(autoscalingv1.CannotSetReference, err)
		return
	}

	klog.V(2).Infof("key=%s resource=%T/%s resource-version=%s setting object reference", original.Name, object, object.Name, newRef.ResourceVersion)

	current.Status.Resources.ServiceRef = newRef
	return
}

func (s *serviceHandler) Equal(this, that *corev1.Service) bool {
	return equality.Semantic.DeepDerivative(&this.Spec, &that.Spec) &&
		equality.Semantic.DeepDerivative(this.GetObjectMeta(), that.GetObjectMeta())
}
