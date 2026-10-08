package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nmoUpgradeOperatorFBCTest struct {
	owned      *helpers.FBCNamespace
	worker     corev1.Node
	config     *unstructured.Unstructured
	configUID  types.UID
	configSpec map[string]interface{}
}

func (hooks *nmoUpgradeOperatorFBCTest) Setup(ctx context.Context) error {
	By("rejecting existing NMO installations and selecting a disposable Ready worker")
	if err := hooks.owned.CheckClean(ctx, "node-maintenance-operator", nmGVK); err != nil {
		return err
	}
	version := &configv1.ClusterVersion{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: "version"}, version); err != nil {
		return err
	}
	if !strings.HasPrefix(version.Status.Desired.Version, "5.0.") {
		return fmt.Errorf("NMO FBC upgrade requires OpenShift 5.0, got %s", version.Status.Desired.Version)
	}
	workers, err := listSchedulableWorkers(ctx)
	if err != nil {
		return err
	}
	if len(workers) < nmoparams.MinWorkerNodesForMaintenance {
		return fmt.Errorf("NMO maintenance requires at least two Ready, schedulable non-control-plane workers")
	}
	hooks.worker = workers[0]
	if err := nmoutils.VerifyNode(&hooks.worker, hooks.worker.UID, false); err != nil {
		return err
	}
	if err := hooks.verifyLeaseAbsent(ctx); err != nil {
		return err
	}
	GinkgoWriter.Printf("NMO mode: upgrade/config persistence, worker drain and recovery; target %s; no reboot\n",
		hooks.worker.Name)

	return hooks.owned.Create(ctx)
}

func (hooks *nmoUpgradeOperatorFBCTest) BeforeUpgrade(ctx context.Context) error {
	By("creating GA maintenance configuration and capturing its UID and complete persisted spec")
	hooks.config = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": nmGVK.GroupVersion().String(), "kind": nmGVK.Kind,
		"spec": map[string]interface{}{
			"nodeName": hooks.worker.Name, "reason": "FBC upgrade persistence " + hooks.owned.Token,
		},
	}}
	hooks.config.SetName("nmo-fbc-" + hooks.owned.Token)
	hooks.config.SetLabels(map[string]string{helpers.FBCRunLabel: hooks.owned.Token})
	Eventually(func() error {
		return APIClient.Create(ctx, hooks.config.DeepCopy(), client.DryRunAll)
	}, medik8sparams.DefaultTimeout, nmoparams.DefaultPollInterval).Should(Succeed())
	node := &corev1.Node{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: hooks.worker.Name}, node); err != nil {
		return err
	}
	if err := nmoutils.VerifyNode(node, hooks.worker.UID, false); err != nil {
		return err
	}
	if err := APIClient.Create(ctx, hooks.config); err != nil {
		return err
	}
	// Record ownership immediately, even if the subsequent read fails.
	hooks.configUID = hooks.config.GetUID()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.config), hooks.config); err != nil {
		return err
	}
	var found bool
	var err error
	hooks.configSpec, found, err = unstructured.NestedMap(hooks.config.Object, "spec")
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("GA NodeMaintenance has no spec")
	}
	hooks.waitForMaintenance(ctx, hooks.configSpec, time.Time{})

	return hooks.reportConfiguration(ctx, "before")
}

func (hooks *nmoUpgradeOperatorFBCTest) AfterUpgrade(ctx context.Context) error {
	By("verifying the original NodeMaintenance UID and complete spec survived the upgrade")
	if err := hooks.reportConfiguration(ctx, "after"); err != nil {
		return err
	}
	By("requiring fresh candidate reconciliation of a reason change, then restoring the original full spec")
	probe := hooks.config.DeepCopy()
	reason := "candidate reconciliation " + hooks.owned.Token
	if err := unstructured.SetNestedField(probe.Object, reason, "spec", "reason"); err != nil {
		return err
	}
	probeSpec, _, err := unstructured.NestedMap(probe.Object, "spec")
	if err != nil {
		return err
	}
	if err := hooks.changeSpecAndReconcile(ctx, probeSpec); err != nil {
		return err
	}
	if err := hooks.changeSpecAndReconcile(ctx, hooks.configSpec); err != nil {
		return err
	}

	return hooks.reportConfiguration(ctx, "after-reconciliation")
}

func (hooks *nmoUpgradeOperatorFBCTest) changeSpecAndReconcile(ctx context.Context, spec map[string]interface{}) error {
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		return err
	}
	// NMO also writes status; retry resource-version conflicts without dropping ownership checks.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.config), hooks.config); err != nil {
			return err
		}
		if err := nmoutils.VerifyOwned(hooks.config, hooks.configUID, hooks.owned.Token, hooks.worker.Name); err != nil {
			return err
		}
		base := hooks.config.DeepCopy()
		if err := unstructured.SetNestedMap(hooks.config.Object, spec, "spec"); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})

		return APIClient.Patch(ctx, hooks.config, patch)
	}); err != nil {
		return err
	}
	// Use the API patch response, not client time or a status sampled before the write.
	since, err := nmoutils.LastUpdate(hooks.config)
	if err != nil {
		return err
	}
	hooks.waitForMaintenance(ctx, spec, since)
	AddReportEntry("nmo-upgrade-fresh-reconciliation", map[string]interface{}{
		"UID": hooks.configUID, "spec": spec, "status": hooks.config.Object["status"],
	})

	return nil
}

func (hooks *nmoUpgradeOperatorFBCTest) waitForMaintenance(
	ctx context.Context, spec map[string]interface{}, since time.Time,
) {
	GinkgoHelper()
	Eventually(func() error {
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.config), hooks.config); err != nil {
			return err
		}
		if err := helpers.VerifyConfigurationPreserved(ctx, APIClient, hooks.config, hooks.configUID, spec); err != nil {
			return err
		}
		if err := nmoutils.VerifyMaintenance(hooks.config, since); err != nil {
			return err
		}
		node := &corev1.Node{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: hooks.worker.Name}, node); err != nil {
			return err
		}

		return nmoutils.VerifyNode(node, hooks.worker.UID, true)
	}, nmoparams.MaintenanceTimeout, nmoparams.DefaultPollInterval).Should(Succeed())
}

func (hooks *nmoUpgradeOperatorFBCTest) reportConfiguration(ctx context.Context, phase string) error {
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.config), hooks.config); err != nil {
		return err
	}
	err := helpers.VerifyConfigurationPreserved(ctx, APIClient, hooks.config, hooks.configUID, hooks.configSpec)
	if err != nil {
		return err
	}
	AddReportEntry("nmo-upgrade-config-"+phase, map[string]interface{}{
		"UID": hooks.configUID, "spec": hooks.configSpec, "nodeUID": hooks.worker.UID,
	})

	return nil
}

func (hooks *nmoUpgradeOperatorFBCTest) verifyLeaseAbsent(ctx context.Context) error {
	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: nmoparams.LeaseNamespace, Name: "node-" + hooks.worker.Name}
	err := APIClient.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	return fmt.Errorf("maintenance lease %s/%s exists", lease.Namespace, lease.Name)
}

func (hooks *nmoUpgradeOperatorFBCTest) Cleanup(ctx context.Context) {
	By("releasing only the owned maintenance CR and verifying worker recovery before removing NMO")
	if hooks.configUID != "" {
		node := &corev1.Node{}
		Expect(APIClient.Get(ctx, client.ObjectKey{Name: hooks.worker.Name}, node)).To(Succeed())
		Expect(node.UID).To(Equal(hooks.worker.UID), "refusing to uncordon a replacement node")
		err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.config), hooks.config)
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
			Expect(nmoutils.VerifyOwned(hooks.config, hooks.configUID, hooks.owned.Token, hooks.worker.Name)).To(Succeed())
			Expect(APIClient.Delete(ctx, hooks.config, client.Preconditions{UID: &hooks.configUID})).To(Succeed())
		}
		Eventually(func() error {
			err := APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.config), hooks.config)
			if !apierrors.IsNotFound(err) {
				if err != nil {
					return err
				}

				return fmt.Errorf("owned NodeMaintenance still exists")
			}
			if err := APIClient.Get(ctx, client.ObjectKey{Name: hooks.worker.Name}, node); err != nil {
				return err
			}
			if err := nmoutils.VerifyNode(node, hooks.worker.UID, false); err != nil {
				return err
			}

			return hooks.verifyLeaseAbsent(ctx)
		}, nmoparams.MaintenanceTimeout, nmoparams.DefaultPollInterval).Should(Succeed())
		AddReportEntry("nmo-upgrade-worker-recovered", map[string]interface{}{"name": node.Name, "UID": node.UID})
	}
	Expect(hooks.owned.Cleanup(ctx)).To(Succeed())
}

func (hooks *nmoUpgradeOperatorFBCTest) FailureEvidence(ctx context.Context) interface{} {
	evidence := map[string]interface{}{"targetWorker": hooks.worker.Name, "nodeUID": hooks.worker.UID}
	if hooks.config != nil {
		current := hooks.config.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			evidence["configError"] = err.Error()
		} else {
			evidence["config"] = current
		}
	}
	node := &corev1.Node{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: hooks.worker.Name}, node); err != nil {
		evidence["nodeError"] = err.Error()
	} else {
		evidence["node"] = node
	}

	return evidence
}
