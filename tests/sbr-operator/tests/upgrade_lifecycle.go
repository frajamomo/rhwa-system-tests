package tests

import (
	"context"
	"fmt"
	"strings"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type sbrUpgradeOperatorFBCTest struct {
	inputs      medik8sparams.FBCUpgradeInputs
	owned       *sbrutils.OwnedRun
	remediation sbrparams.FBCRemediationInputs
	configUID   types.UID
	configSpec  map[string]interface{}
	targetNode  string
}

func (hooks *sbrUpgradeOperatorFBCTest) Setup(ctx context.Context) error {
	var err error
	hooks.remediation, err = sbrparams.LoadFBCRemediationInputs()
	if err != nil {
		return err
	}
	AddReportEntry("sbr-upgrade-remediation-inputs", hooks.remediation)

	By("rejecting pre-existing SBR installations and resources")
	if err := sbrutils.CheckClean(ctx, APIClient, hooks.owned.Namespace); err != nil {
		return err
	}
	version := &configv1.ClusterVersion{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: "version"}, version); err != nil {
		return err
	}
	if !strings.HasPrefix(version.Status.Desired.Version, "5.0.") {
		return fmt.Errorf("SBR FBC upgrade requires OpenShift 5.0, got %s", version.Status.Desired.Version)
	}
	if hooks.remediation.Enabled {
		class := &storagev1.StorageClass{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: hooks.remediation.StorageClass}, class); err != nil {
			return fmt.Errorf("read remediation StorageClass: %w", err)
		}
	}

	err = hooks.owned.CreateNamespace(ctx)
	if err != nil && !hooks.inputs.SkipCleanup {
		// The shared suite registers cleanup only after Setup succeeds.
		hooks.Cleanup(ctx)
	}

	return err
}

func (hooks *sbrUpgradeOperatorFBCTest) BeforeUpgrade(ctx context.Context) error {
	By("creating the no-storage config whose identity and specification must survive the upgrade")
	if err := waitForUpgradeAPI(ctx, upgradeSBRC()); err != nil {
		return err
	}
	object := buildSBRC(sbrparams.SBRUpgradeConfigTestName, sbrutils.SafeSpec(hooks.owned.Token, ""))
	if err := hooks.owned.Create(ctx, object); err != nil {
		return err
	}
	if err := waitForSBRStorageValidation(ctx, object.GetUID(), "no shared storage configured"); err != nil {
		return fmt.Errorf("wait for baseline storage validation: %w", err)
	}
	uid, spec := captureSBRCConfiguration(ctx)
	Expect(types.UID(uid)).To(Equal(object.GetUID()), "baseline config must not be replaced")
	hooks.configUID, hooks.configSpec = types.UID(uid), spec
	AddReportEntry("sbr-config-before-operator-upgrade", map[string]interface{}{"uid": uid, "spec": spec})
	GinkgoWriter.Printf("SBR config before operator upgrade: uid=%s spec=%v\n", uid, spec)
	assertSBRProbeHasNoOperands(ctx)

	return nil
}

func (hooks *sbrUpgradeOperatorFBCTest) AfterUpgrade(ctx context.Context) error {
	// The shared FBC runner has already verified the candidate CSV and controller
	// image, resolving tagged pod pullspecs through their runtime image digests.
	By("verifying the same StorageBasedRemediationConfig UID and complete spec survived")
	hooks.verifyConfiguration(ctx)

	By("requiring a fresh candidate-controller response to a unique missing StorageClass")
	className := "sbr-upgrade-probe-" + hooks.owned.Token
	err := APIClient.Get(ctx, client.ObjectKey{Name: className}, &storagev1.StorageClass{})
	if err == nil {
		return fmt.Errorf("probe StorageClass must not exist: %s", className)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read probe StorageClass %s: %w", className, err)
	}
	probe := upgradeSBRC()
	Expect(unstructured.SetNestedMap(probe.Object, hooks.configSpec, "spec")).To(Succeed())
	Expect(unstructured.SetNestedField(probe.Object, className, "spec", "sharedStorageClass")).To(Succeed())
	probeSpec, _, err := unstructured.NestedMap(probe.Object, "spec")
	Expect(err).NotTo(HaveOccurred())
	hooks.patchConfiguration(ctx, probeSpec)
	validationErr := waitForSBRStorageValidation(
		ctx, hooks.configUID, fmt.Sprintf("StorageClass '%s' not found", className))

	By("restoring the original specification without replacing the config")
	hooks.patchConfiguration(ctx, hooks.configSpec)
	if validationErr != nil {
		return fmt.Errorf("wait for fresh candidate storage validation: %w", validationErr)
	}
	hooks.verifyConfiguration(ctx)
	assertSBRProbeHasNoOperands(ctx)
	AddReportEntry("sbr-upgrade-fresh-reconciliation", map[string]interface{}{
		"uid": hooks.configUID, "probeStorageClass": className, "response": "PVCError: missing StorageClass",
	})
	AddReportEntry("sbr-config-after-operator-upgrade", map[string]interface{}{
		"uid": hooks.configUID, "spec": hooks.configSpec,
	})
	GinkgoWriter.Printf("SBR config after operator upgrade: uid=%s spec=%v\n", hooks.configUID, hooks.configSpec)

	if !hooks.remediation.Enabled {
		AddReportEntry("sbr-upgrade-remediation", "not requested: upgrade/configuration-only; no storage or node reboot")
		GinkgoWriter.Println("SBR remediation NOT REQUESTED: upgrade/configuration-only run; no storage or node reboot")

		return nil
	}

	return hooks.runRemediationCycle(ctx)
}

func (hooks *sbrUpgradeOperatorFBCTest) verifyConfiguration(ctx context.Context) {
	object := upgradeSBRC()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
	Expect(helpers.VerifyConfigurationPreserved(ctx, APIClient, object, hooks.configUID, hooks.configSpec)).To(Succeed())
}

func (hooks *sbrUpgradeOperatorFBCTest) patchConfiguration(ctx context.Context, spec map[string]interface{}) {
	Eventually(func() error {
		return hooks.writeConfiguration(ctx, spec)
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
}

func (hooks *sbrUpgradeOperatorFBCTest) writeConfiguration(ctx context.Context, spec map[string]interface{}) error {
	object := upgradeSBRC()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return err
	}
	if object.GetUID() != hooks.configUID || object.GetLabels()[sbrutils.RunLabel] != hooks.owned.Token {
		return fmt.Errorf("refusing to patch replaced/unowned StorageBasedRemediationConfig")
	}
	original := object.DeepCopy()
	if err := unstructured.SetNestedMap(object.Object, spec, "spec"); err != nil {
		return err
	}
	// Optimistic locking protects identity and retries handle webhook rotation.
	return APIClient.Patch(ctx, object,
		client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func waitForSBRStorageValidation(ctx context.Context, uid types.UID, message string) error {
	return helpers.WaitForEvents(ctx, APIClient.K8sClient, helpers.InvolvedObjectRef{
		Kind: "StorageBasedRemediationConfig", Name: sbrparams.SBRUpgradeConfigTestName,
		Namespace: medik8sparams.OperatorNs, UID: string(uid),
	}, []helpers.EventExpectation{{Reason: "PVCError", Type: corev1.EventTypeWarning, MessageSubstr: message}},
		medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval)
}

func assertSBRProbeHasNoOperands(ctx context.Context) {
	claims := &corev1.PersistentVolumeClaimList{}
	Expect(APIClient.List(ctx, claims, client.InNamespace(medik8sparams.OperatorNs))).To(Succeed())
	Expect(claims.Items).To(BeEmpty(), "no-storage probe must not provision a PVC")
	ds, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
		ctx, upgradeSBRCAgentDaemonSetName, metav1.GetOptions{})
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no-storage probe must not create an agent DaemonSet: %v", ds)
}

func (hooks *sbrUpgradeOperatorFBCTest) Cleanup(ctx context.Context) {
	// A failed probe may still point at its intentionally missing StorageClass.
	// Restore before deletion so SBRC cleanup never attempts a storage-cleanup Job.
	if hooks.configUID != "" {
		if err := hooks.writeConfiguration(ctx, hooks.configSpec); err != nil && !apierrors.IsNotFound(err) {
			AddReportEntry("sbr-upgrade-restore-failure", err.Error())
		}
	}
	if err := hooks.owned.Cleanup(ctx); err != nil {
		AddReportEntry("sbr-upgrade-cleanup-failure", err.Error())
		AddReportEntry("sbr-upgrade-cleanup-evidence", hooks.FailureEvidence(ctx))
	}
	if hooks.targetNode != "" {
		if err := helpers.WaitForNodeReady(ctx, APIClient, hooks.targetNode,
			sbrparams.NodeRebootPollInterval, sbrparams.NodeRebootTimeout, GinkgoWriter.Printf); err != nil {
			AddReportEntry("sbr-upgrade-node-recovery-failure", err.Error())
		}
	}
}

func (hooks *sbrUpgradeOperatorFBCTest) FailureEvidence(ctx context.Context) interface{} {
	return sbrutils.CollectFailureEvidence(ctx, hooks.owned.Namespace)
}
