package tests

import (
	"context"
	"fmt"

	"github.com/medik8s/system-tests/tests/far-operator/internal/farparams"
	"github.com/medik8s/system-tests/tests/far-operator/internal/farutils"
	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (hooks *farUpgradeOperatorFBCTest) selectRemediationWorker(ctx context.Context) (*corev1.Node, error) {
	leaderNode, err := farutils.GetActiveFARControllerNode(ctx, APIClient)
	if err != nil {
		return nil, err
	}
	excluded := []string{leaderNode}
	nodes := &corev1.NodeList{}
	if err := APIClient.List(ctx, nodes); err != nil {
		return nil, err
	}
	for _, node := range nodes.Items {
		_, master := node.Labels["node-role.kubernetes.io/master"]
		_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
		if master || controlPlane || node.Labels["remediation.medik8s.io/exclude-from-remediation"] == "true" {
			excluded = append(excluded, node.Name)
		}
	}

	return helpers.SelectWorkerNode(ctx, APIClient, excluded...)
}

func (hooks *farUpgradeOperatorFBCTest) runRemediationCycle(ctx context.Context, phase string) error {
	var node *corev1.Node
	Eventually(func() error {
		var err error
		node, err = hooks.selectRemediationWorker(ctx)

		return err
	}, farparams.ControllerHandoverTimeout, farparams.DefaultPollInterval).Should(Succeed())
	if node.UID == "" || node.Status.NodeInfo.BootID == "" {
		return fmt.Errorf("target worker must have a UID and boot ID")
	}
	hooks.targetNode = node.Name
	// A standalone pod lets us observe actual eviction without deploying storage or another operator.
	workloadName, err := helpers.CreateWorkloadPod(ctx, APIClient, node.Name, hooks.owned.Name,
		farparams.WorkloadTestImage, "far-fbc-workload-", farparams.WorkloadPodReadyTimeout, farparams.DefaultPollInterval)
	if err != nil {
		return err
	}
	workload := &corev1.Pod{}
	workloadKey := client.ObjectKey{Name: workloadName, Namespace: hooks.owned.Name}
	if err := APIClient.Get(ctx, workloadKey, workload); err != nil {
		return err
	}
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(hooks.template), hooks.template)).To(Succeed())
	Expect(helpers.VerifyConfigurationPreserved(
		ctx, APIClient, hooks.template, hooks.configUID, hooks.configSpec,
	)).To(Succeed())
	remediation, err := farutils.RemediationFromTemplate(hooks.template, node.Name, hooks.owned.Token)
	if err != nil {
		return err
	}
	if err := APIClient.Create(ctx, remediation); err != nil {
		return err
	}
	hooks.remediation = remediation
	GinkgoWriter.Printf("FAR %s-upgrade remediation: node=%s uid=%s bootID=%s CRUID=%s\n",
		phase, node.Name, node.UID, node.Status.NodeInfo.BootID, remediation.GetUID())
	Eventually(func() error {
		current := &corev1.Node{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: node.Name}, current); err != nil {
			return err
		}
		if current.UID != node.UID || current.Status.NodeInfo.BootID == "" ||
			current.Status.NodeInfo.BootID == node.Status.NodeInfo.BootID {
			return fmt.Errorf("worker has not rebooted with its original identity")
		}
		live := remediation.DeepCopy()
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(live), live); err != nil {
			return err
		}
		if err := farutils.VerifyOwnedObject(live, remediation.GetUID(), hooks.owned.Token); err != nil {
			return err
		}
		if !farConditionSucceeded(live) {
			return fmt.Errorf("fresh FAR remediation has not succeeded")
		}

		return nil
	}, farparams.NodeRebootTimeout, farparams.DefaultPollInterval).Should(Succeed())
	Eventually(func() bool {
		current := &corev1.Pod{}
		err := APIClient.Get(ctx, workloadKey, current)

		return farutils.WorkloadEvicted(workload.UID, current.UID, current.DeletionTimestamp != nil, err)
	}, farparams.WorkloadEvictionTimeout, farparams.DefaultPollInterval).Should(BeTrue())
	Expect(hooks.deleteOwnedRemediation(ctx)).To(Succeed())
	hooks.waitForWorkerRecovery(ctx, node)
	current := &corev1.Node{}
	Expect(APIClient.Get(ctx, client.ObjectKey{Name: node.Name}, current)).To(Succeed())
	AddReportEntry("far-"+phase+"-upgrade-remediation", map[string]interface{}{
		"node": node.Name, "nodeUID": node.UID, "beforeBootID": node.Status.NodeInfo.BootID,
		"afterBootID": current.Status.NodeInfo.BootID, "ready": true, "workloadEvicted": true,
	})
	hooks.targetNode = ""
	if phase == "after" {
		return helpers.WaitForDeploymentImage(ctx, APIClient, hooks.owned.Name,
			farparams.OperatorDeploymentName, farparams.ManagerContainerName, hooks.inputs.CandidateImage,
			medik8sparams.DefaultTimeout, farparams.DefaultPollInterval)
	}

	return nil
}

func (hooks *farUpgradeOperatorFBCTest) waitForWorkerRecovery(ctx context.Context, node *corev1.Node) {
	GinkgoHelper()
	Eventually(func() error {
		current := &corev1.Node{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: node.Name}, current); err != nil {
			return err
		}
		if current.UID != node.UID || !helpers.IsNodeReady(current) {
			return fmt.Errorf("original worker has not recovered")
		}
		for _, taint := range current.Spec.Taints {
			if taint.Key == farparams.FARNoScheduleTaintKey || taint.Key == "node.kubernetes.io/out-of-service" {
				return fmt.Errorf("worker still has a remediation taint")
			}
		}

		return nil
	}, farparams.NodeReadyTimeout, farparams.DefaultPollInterval).Should(Succeed())
}

func (hooks *farUpgradeOperatorFBCTest) deleteOwnedRemediation(ctx context.Context) error {
	if hooks.remediation == nil {
		return nil
	}
	if err := hooks.owned.VerifyOwner(ctx); err != nil {
		return err
	}
	object := hooks.remediation.DeepCopy()
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		if apierrors.IsNotFound(err) {
			hooks.remediation = nil

			return nil
		}

		return err
	}
	if err := farutils.VerifyOwnedObject(object, hooks.remediation.GetUID(), hooks.owned.Token); err != nil {
		return err
	}
	uid := object.GetUID()
	if err := client.IgnoreNotFound(APIClient.Delete(ctx, object, client.Preconditions{UID: &uid})); err != nil {
		return err
	}
	if err := wait.PollUntilContextTimeout(ctx, farparams.DefaultPollInterval,
		farparams.RemediationCRDeletionTimeout, true, func(ctx context.Context) (bool, error) {
			err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)

			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}); err != nil {
		return err
	}
	hooks.remediation = nil

	return nil
}
