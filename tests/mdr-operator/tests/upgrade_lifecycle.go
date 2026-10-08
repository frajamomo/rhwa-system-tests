package tests

import (
	"context"
	"fmt"
	"strings"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/mdr-operator/internal/mdrparams"
	"github.com/medik8s/system-tests/tests/mdr-operator/internal/mdrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type mdrUpgradeOperatorFBCTest struct {
	owned      *helpers.FBCNamespace
	template   *unstructured.Unstructured
	configUID  types.UID
	configSpec map[string]interface{}
	probe      *unstructured.Unstructured
}

func (hooks *mdrUpgradeOperatorFBCTest) Setup(ctx context.Context) error {
	By("rejecting existing MDR installations and acquiring an owned namespace")
	if err := hooks.owned.CheckClean(ctx, "machine-deletion-remediation", mdrGVK, mdrtGVK); err != nil {
		return err
	}
	version := &configv1.ClusterVersion{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: "version"}, version); err != nil {
		return err
	}
	if !strings.HasPrefix(version.Status.Desired.Version, "5.0.") {
		return fmt.Errorf("MDR FBC upgrade requires OpenShift 5.0, got %s", version.Status.Desired.Version)
	}
	GinkgoWriter.Println("MDR mode: upgrade/template persistence and safe reconciliation; Machine deletion NOT REQUESTED")

	return hooks.owned.Create(ctx)
}

func (hooks *mdrUpgradeOperatorFBCTest) BeforeUpgrade(ctx context.Context) error {
	By("creating a GA MDR template and capturing its UID and complete persisted spec")
	hooks.template = buildMDRT("mdr-fbc-upgrade-config")
	hooks.template.SetLabels(map[string]string{helpers.FBCRunLabel: hooks.owned.Token})
	Eventually(func() error {
		return APIClient.Create(ctx, hooks.template.DeepCopy(), client.DryRunAll)
	}, medik8sparams.DefaultTimeout, mdrparams.DefaultPollInterval).Should(Succeed())
	if err := APIClient.Create(ctx, hooks.template); err != nil {
		return err
	}
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.template), hooks.template)).To(Succeed())
	hooks.configUID = hooks.template.GetUID()
	var found bool
	var err error
	hooks.configSpec, found, err = unstructured.NestedMap(hooks.template.Object, "spec")
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("GA MDR template has no spec")
	}
	Expect(hooks.reportConfiguration(ctx, "before")).To(Succeed())

	return hooks.runSafeProbe(ctx, "before")
}

func (hooks *mdrUpgradeOperatorFBCTest) AfterUpgrade(ctx context.Context) error {
	By("verifying the original MDR template UID and complete spec survived the upgrade")
	if err := hooks.reportConfiguration(ctx, "after"); err != nil {
		return err
	}
	By("requiring a fresh request to be reconciled by the candidate without deleting a Machine")
	if err := hooks.runSafeProbe(ctx, "after"); err != nil {
		return err
	}

	return hooks.reportConfiguration(ctx, "after-reconciliation")
}

func (hooks *mdrUpgradeOperatorFBCTest) reportConfiguration(ctx context.Context, phase string) error {
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.template), hooks.template); err != nil {
		return err
	}
	err := helpers.VerifyConfigurationPreserved(ctx, APIClient, hooks.template, hooks.configUID, hooks.configSpec)
	if err != nil {
		return err
	}
	AddReportEntry("mdr-upgrade-config-"+phase, map[string]interface{}{
		"UID": hooks.template.GetUID(), "spec": hooks.configSpec,
	})

	return nil
}

func (hooks *mdrUpgradeOperatorFBCTest) runSafeProbe(ctx context.Context, phase string) error {
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		return err
	}
	name := "mdr-fbc-" + phase + "-" + hooks.owned.Token
	if err := APIClient.Get(ctx, client.ObjectKey{Name: name}, &corev1.Node{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}

		return fmt.Errorf("refusing MDR probe targeting existing node %s", name)
	}
	var err error
	hooks.probe, err = mdrutils.SafeProbe(hooks.template, name, hooks.owned.Token)
	if err != nil {
		return err
	}
	if err := APIClient.Create(ctx, hooks.probe); err != nil {
		return err
	}
	uid := hooks.probe.GetUID()
	Eventually(func() error {
		current := hooks.probe.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			return err
		}

		return mdrutils.VerifySafeProbe(current, uid, hooks.owned.Token)
	}, medik8sparams.DefaultTimeout, mdrparams.DefaultPollInterval).Should(Succeed())
	AddReportEntry("mdr-upgrade-fresh-reconciliation-"+phase, map[string]interface{}{
		"requestUID": uid, "templateUID": hooks.configUID,
		"reason": mdrparams.ConditionReasonStoppedByNHC, "machineDeletionRequested": false,
	})
	if err := APIClient.Delete(ctx, hooks.probe, client.Preconditions{UID: &uid}); err != nil {
		return err
	}
	hooks.probe = nil

	return nil
}

func (hooks *mdrUpgradeOperatorFBCTest) Cleanup(ctx context.Context) {
	By("cleaning only this run's MDR namespace and OLM-owned resources")
	Expect(hooks.owned.Cleanup(ctx)).To(Succeed())
}

func (hooks *mdrUpgradeOperatorFBCTest) FailureEvidence(ctx context.Context) interface{} {
	evidence := map[string]interface{}{"machineDeletionRequested": false}
	for name, object := range map[string]*unstructured.Unstructured{"template": hooks.template, "probe": hooks.probe} {
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

	return evidence
}
