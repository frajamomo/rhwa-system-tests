package tests

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/medik8s/system-tests/tests/far-operator/internal/farparams"
	"github.com/medik8s/system-tests/tests/far-operator/internal/farutils"
	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type farUpgradeOperatorFBCTest struct {
	inputs             medik8sparams.FBCUpgradeInputs
	owned              *helpers.FBCNamespace
	region             string
	template           *unstructured.Unstructured
	configUID          types.UID
	configSpec         map[string]interface{}
	credentials        *unstructured.Unstructured
	remediation        *unstructured.Unstructured
	targetNode         string
	remediationEnabled bool
}

func (hooks *farUpgradeOperatorFBCTest) Setup(ctx context.Context) error {
	By("rejecting existing FAR installations and acquiring an owned namespace")
	if err := hooks.owned.CheckClean(ctx, "fence-agents-remediation", farGVK, farTemplateGVK); err != nil {
		return err
	}
	version := &configv1.ClusterVersion{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: "version"}, version); err != nil {
		return err
	}
	if !strings.HasPrefix(version.Status.Desired.Version, "5.0.") {
		return fmt.Errorf("FAR FBC upgrade requires OpenShift 5.0, got %s", version.Status.Desired.Version)
	}
	enabled, err := farutils.FBCRemediationEnabled(os.Getenv("FAR_FBC_REMEDIATION"))
	if err != nil {
		return err
	}
	hooks.remediationEnabled = enabled
	if !enabled {
		GinkgoWriter.Println("FAR mode: upgrade/configuration persistence; real fencing NOT REQUESTED")

		return hooks.owned.Create(ctx)
	}
	platform, region, err := helpers.DetectPlatform(ctx, APIClient)
	if err != nil {
		return err
	}
	if platform != configv1.AWSPlatformType || region == "" {
		return fmt.Errorf("FAR upgrade remediation requires AWS and a nonempty region")
	}
	hooks.region = region
	count, err := helpers.CountReadyWorkerNodes(ctx, APIClient)
	if err != nil {
		return err
	}
	if count < farparams.MinWorkersForDestructiveTests {
		return fmt.Errorf("FAR upgrade requires at least %d Ready workers, got %d",
			farparams.MinWorkersForDestructiveTests, count)
	}
	if farparams.WorkloadTestImage == "" || strings.HasPrefix(farparams.WorkloadTestImage, "unused") {
		return fmt.Errorf("WORKLOAD_IMAGE must be a runnable image with sleep for eviction validation")
	}

	return hooks.owned.Create(ctx)
}

func (hooks *farUpgradeOperatorFBCTest) BeforeUpgrade(ctx context.Context) error {
	hooks.template = farutils.SafeUpgradeTemplate("far-fbc-upgrade-config", hooks.owned.Name, hooks.owned.Token)
	if hooks.remediationEnabled {
		if err := hooks.prepareAWSTemplate(ctx); err != nil {
			return err
		}
	}

	By("creating a customized GA template and capturing its complete defaulted specification")
	hooks.template.SetLabels(map[string]string{helpers.FBCRunLabel: hooks.owned.Token})
	// A dry-run still invokes admission; retry until the GA webhook actually accepts this template.
	Eventually(func() error {
		return APIClient.Create(ctx, hooks.template.DeepCopy(), client.DryRunAll)
	}, farparams.WebhookReadyTimeout, farparams.DefaultPollInterval).Should(Succeed())
	if err := APIClient.Create(ctx, hooks.template); err != nil {
		return err
	}
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.template), hooks.template)).To(Succeed())
	hooks.configUID = hooks.template.GetUID()
	var err error
	hooks.configSpec, _, err = unstructured.NestedMap(hooks.template.Object, "spec")
	if err != nil {
		return err
	}
	hooks.reportConfiguration(ctx, "before")
	if !hooks.remediationEnabled {
		return nil
	}

	By("requiring a GA worker reboot, workload eviction, and recovery using the customized template")

	return hooks.runRemediationCycle(ctx, "before")
}

func (hooks *farUpgradeOperatorFBCTest) prepareAWSTemplate(ctx context.Context) error {
	By("provisioning least-privilege AWS fencing credentials without printing their values")
	if err := hooks.provisionCredentials(ctx); err != nil {
		return err
	}
	parameters, err := farutils.BuildAWSNodeParameters(ctx, APIClient)
	if err != nil {
		return err
	}
	nodeParams := map[string]interface{}{}
	for name, nodes := range parameters {
		values := map[string]interface{}{}
		for node, instance := range nodes {
			values[node] = instance
		}
		nodeParams[name] = values
	}

	hooks.template = buildFARTemplateUnstructured("far-fbc-upgrade-config", farparams.FenceAgentAWS,
		map[string]interface{}{"--region": hooks.region, "--action": "reboot", "--skip-race-check": ""}, nodeParams)
	spec, _, err := unstructured.NestedMap(hooks.template.Object, "spec", "template", "spec")
	if err != nil {
		return err
	}
	spec["retrycount"] = int64(7)
	spec["retryinterval"] = "7s"
	spec["timeout"] = "45s"
	spec["remediationStrategy"] = "ResourceDeletion"

	return unstructured.SetNestedMap(hooks.template.Object, spec, "spec", "template", "spec")
}

func (hooks *farUpgradeOperatorFBCTest) AfterUpgrade(ctx context.Context) error {
	By("verifying the original FAR template UID and complete spec survived")
	hooks.reportConfiguration(ctx, "after")
	probe := hooks.template.DeepCopy()
	Expect(unstructured.SetNestedMap(probe.Object, hooks.configSpec, "spec")).To(Succeed())
	Expect(unstructured.SetNestedField(probe.Object, int64(1), "spec", "statusValidationSample")).To(Succeed())
	probeSpec, _, err := unstructured.NestedMap(probe.Object, "spec")
	if err != nil {
		return err
	}

	By("requiring fresh candidate template reconciliation at the current generation")
	if !hooks.remediationEnabled {
		GinkgoWriter.Println("Safe status probe: ValidationFailed is EXPECTED for the dummy endpoint; no node is fenced")
	}
	hooks.patchConfiguration(ctx, probeSpec)
	Eventually(func() error {
		object := hooks.template.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			return err
		}
		if err := helpers.VerifyConfigurationPreserved(ctx, APIClient, object, hooks.configUID, probeSpec); err != nil {
			return err
		}

		return farutils.VerifyTemplateValidation(object, hooks.remediationEnabled)
	}, farparams.EventVerifyTimeout, farparams.DefaultPollInterval).Should(Succeed())
	AddReportEntry("far-upgrade-fresh-reconciliation", map[string]interface{}{
		"templateUID": hooks.configUID, "statusValidationSample": 1, "observedCurrentGeneration": true,
		"realFencingRequested": hooks.remediationEnabled,
	})

	By("restoring and verifying the original configuration without recreating it")
	hooks.patchConfiguration(ctx, hooks.configSpec)
	Eventually(func() error {
		object := hooks.template.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			return err
		}
		conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
		if err != nil {
			return err
		}
		for _, value := range conditions {
			condition, ok := value.(map[string]interface{})
			if ok && condition["type"] == farutils.TemplateValidationCondition {
				return fmt.Errorf("template validation probe has not been cleared")
			}
		}

		return nil
	}, farparams.FARConditionTimeout, farparams.DefaultPollInterval).Should(Succeed())
	hooks.reportConfiguration(ctx, "restored")
	if !hooks.remediationEnabled {
		return nil
	}

	By("requiring a candidate worker reboot, workload eviction, and recovery using the preserved template")

	return hooks.runRemediationCycle(ctx, "after")
}

func (hooks *farUpgradeOperatorFBCTest) reportConfiguration(ctx context.Context, phase string) {
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.template), hooks.template)).To(Succeed())
	Expect(helpers.VerifyConfigurationPreserved(
		ctx, APIClient, hooks.template, hooks.configUID, hooks.configSpec,
	)).To(Succeed())
	AddReportEntry("far-config-"+phase+"-operator-upgrade", map[string]interface{}{
		"uid": hooks.configUID, "spec": hooks.configSpec,
	})
	GinkgoWriter.Printf("FAR template %s operator upgrade: uid=%s spec=%v\n", phase, hooks.configUID, hooks.configSpec)
}

func (hooks *farUpgradeOperatorFBCTest) patchConfiguration(ctx context.Context, spec map[string]interface{}) {
	Eventually(func() error {
		return hooks.writeConfiguration(ctx, spec)
	}, medik8sparams.DefaultTimeout, farparams.DefaultPollInterval).Should(Succeed())
}

func (hooks *farUpgradeOperatorFBCTest) writeConfiguration(ctx context.Context, spec map[string]interface{}) error {
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		return err
	}
	object := hooks.template.DeepCopy()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return err
	}
	if hooks.configUID == "" || object.GetUID() != hooks.configUID {
		return fmt.Errorf("refusing to patch a replaced/unowned FAR template")
	}
	original := object.DeepCopy()
	if err := unstructured.SetNestedMap(object.Object, spec, "spec"); err != nil {
		return err
	}

	return APIClient.Patch(ctx, object,
		client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (hooks *farUpgradeOperatorFBCTest) Cleanup(ctx context.Context) {
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		AddReportEntry("far-upgrade-cleanup-owner-check", err.Error())

		return
	}
	if err := hooks.deleteOwnedRemediation(ctx); err != nil {
		AddReportEntry("far-upgrade-remediation-cleanup-failure", err.Error())
	}
	if hooks.targetNode != "" {
		if err := farutils.WaitForNodeReady(ctx, APIClient, hooks.targetNode,
			farparams.NodeReadyTimeout, GinkgoWriter.Printf); err != nil {
			AddReportEntry("far-upgrade-recovery-failure", err.Error())
		}
	}
	if hooks.configSpec != nil {
		if err := hooks.writeConfiguration(ctx, hooks.configSpec); err != nil {
			AddReportEntry("far-upgrade-config-restore-failure", err.Error())
		}
	}
	if err := hooks.deleteCredentialsRequest(ctx); err != nil {
		AddReportEntry("far-upgrade-credentials-cleanup-failure", err.Error())
	}
	if err := hooks.owned.Cleanup(ctx); err != nil {
		AddReportEntry("far-upgrade-cleanup-failure", err.Error())
	}
}

func (hooks *farUpgradeOperatorFBCTest) FailureEvidence(ctx context.Context) interface{} {
	evidence := map[string]interface{}{"targetNode": hooks.targetNode}
	for name, object := range map[string]*unstructured.Unstructured{
		"template": hooks.template, "remediation": hooks.remediation,
	} {
		if object == nil {
			continue
		}
		current := object.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			evidence[name+"Error"] = err.Error()
		} else {
			evidence[name] = current
		}
	}
	if hooks.targetNode != "" {
		node := &corev1.Node{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: hooks.targetNode}, node); err == nil {
			evidence["nodeUID"] = node.UID
			evidence["bootID"] = node.Status.NodeInfo.BootID
			evidence["conditions"] = node.Status.Conditions
		} else if !apierrors.IsNotFound(err) {
			evidence["nodeError"] = err.Error()
		}
	}
	// Never dump Secret data or resolved fence-agent command arguments.
	return evidence
}
