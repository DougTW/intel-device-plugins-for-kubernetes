// Copyright 2020 Intel Corporation. All Rights Reserved.
//
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

// Package fpga implements E2E tests for FPGA device plugin.
package fpga

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/intel/intel-device-plugins-for-kubernetes/test/e2e/utils"
	"github.com/onsi/ginkgo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

const (
	pluginKustomizationYaml  = "deployments/fpga_plugin/base/kustomization.yaml"
	webhookKustomizationYaml = "deployments/fpga_admissionwebhook/default/kustomization.yaml"
	nlb0NodeResource         = "fpga.intel.com/af-695.d84.aVKNtusxV3qMNmj5-qCB9thCTcSko8QT-J5DNoP5BAs"
	nlb0PodResource          = "fpga.intel.com/arria10.dcp1.2-nlb0-orchestrated"
	nlb3PodResource          = "fpga.intel.com/arria10.dcp1.2-nlb3-orchestrated"
	nlb0PodResourceAF        = "fpga.intel.com/arria10.dcp1.2-nlb0-preprogrammed"
	arria10NodeResource      = "fpga.intel.com/region-69528db6eb31577a8c3668f9faa081f6"
)

func init() {
	ginkgo.Describe("FPGA Plugin E2E tests", describe)
}

func describe() {
	webhookKustomizationPath, err := utils.LocateRepoFile(webhookKustomizationYaml)
	if err != nil {
		framework.Failf("unable to locate %q: %v", webhookKustomizationYaml, err)
	}

	pluginKustomizationPath, err := utils.LocateRepoFile(pluginKustomizationYaml)
	if err != nil {
		framework.Failf("unable to locate %q: %v", pluginKustomizationYaml, err)
	}

	fmw := framework.NewDefaultFramework("fpgaplugin-e2e")

	ginkgo.It("Run FPGA plugin tests", func() {
		// Deploy webhook
		ginkgo.By(fmt.Sprintf("namespace %s: deploying webhook", fmw.Namespace.Name))
		utils.DeployFpgaWebhook(fmw, webhookKustomizationPath)

		ginkgo.By("deploying mappings")
		framework.RunKubectlOrDie(fmw.Namespace.Name, "apply", "-n", fmw.Namespace.Name, "-f", filepath.Dir(webhookKustomizationPath)+"/../mappings-collection.yaml")

		// Run region test case twice to ensure that device is reprogrammed at least once
		runTestCase(fmw, pluginKustomizationPath, "region", arria10NodeResource, nlb3PodResource, "nlb3", "nlb0")
		runTestCase(fmw, pluginKustomizationPath, "region", arria10NodeResource, nlb0PodResource, "nlb0", "nlb3")
		// Run af test case
		runTestCase(fmw, pluginKustomizationPath, "af", nlb0NodeResource, nlb0PodResourceAF, "nlb0", "nlb3")
	})
}

func runTestCase(fmw *framework.Framework, pluginKustomizationPath, pluginMode, nodeResource, podResource, cmd1, cmd2 string) {
	tmpDir, err := ioutil.TempDir("", "fpgaplugine2etest-"+fmw.Namespace.Name)
	if err != nil {
		framework.Failf("unable to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	err = utils.CreateKustomizationOverlay(fmw.Namespace.Name, filepath.Dir(pluginKustomizationPath)+"/../overlays/"+pluginMode, tmpDir)
	if err != nil {
		framework.Failf("unable to kustomization overlay: %v", err)
	}

	ginkgo.By(fmt.Sprintf("namespace %s: deploying FPGA plugin in %s mode", fmw.Namespace.Name, pluginMode))
	_, _ = framework.RunKubectl(fmw.Namespace.Name, "delete", "-k", tmpDir)
	framework.RunKubectlOrDie(fmw.Namespace.Name, "apply", "-k", tmpDir)

	waitForPod(fmw, "intel-fpga-plugin")

	resource := v1.ResourceName(nodeResource)
	ginkgo.By("checking if the resource is allocatable")
	if err := utils.WaitForNodesWithResource(fmw.ClientSet, resource, 30*time.Second); err != nil {
		framework.Failf("unable to wait for nodes to have positive allocatable resource: %v", err)
	}

	resource = v1.ResourceName(podResource)
	image := "intel/opae-nlb-demo:devel"

	ginkgo.By("submitting a pod requesting correct FPGA resources")
	pod := createPod(fmw, fmt.Sprintf("fpgaplugin-%s-%s-%s-correct", pluginMode, cmd1, cmd2), resource, image, []string{cmd1, "-S0"})

	ginkgo.By("waiting the pod to finish successfully")
	fmw.PodClient().WaitForSuccess(pod.ObjectMeta.Name, 60*time.Second)
	// If WaitForSuccess fails, ginkgo doesn't show the logs of the failed container.
	// Replacing WaitForSuccess with WaitForFinish + 'kubelet logs' would show the logs
	//fmw.PodClient().WaitForFinish(pod.ObjectMeta.Name, 60*time.Second)
	//framework.RunKubectlOrDie(fmw.Namespace.Name, "--namespace", fmw.Namespace.Name, "logs", pod.ObjectMeta.Name)

	ginkgo.By("submitting a pod requesting incorrect FPGA resources")
	pod = createPod(fmw, fmt.Sprintf("fpgaplugin-%s-%s-%s-incorrect", pluginMode, cmd1, cmd2), resource, image, []string{cmd2, "-S0"})

	ginkgo.By("waiting the pod failure")
	utils.WaitForPodFailure(fmw, pod.ObjectMeta.Name, 60*time.Second)
}

func createPod(fmw *framework.Framework, name string, resourceName v1.ResourceName, image string, command []string) *v1.Pod {
	resourceList := v1.ResourceList{resourceName: resource.MustParse("1"),
		"cpu":           resource.MustParse("1"),
		"hugepages-2Mi": resource.MustParse("20Mi")}
	podSpec := fmw.NewTestPod(name, resourceList, resourceList)
	podSpec.Spec.RestartPolicy = v1.RestartPolicyNever
	podSpec.Spec.Containers[0].Image = image
	podSpec.Spec.Containers[0].Command = command
	podSpec.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
		Capabilities: &v1.Capabilities{
			Add: []v1.Capability{"IPC_LOCK"},
		},
	}

	pod, err := fmw.ClientSet.CoreV1().Pods(fmw.Namespace.Name).Create(context.TODO(),
		podSpec, metav1.CreateOptions{})
	framework.ExpectNoError(err, "pod Create API error")
	return pod
}

func waitForPod(fmw *framework.Framework, name string) {
	ginkgo.By(fmt.Sprintf("waiting for %s availability", name))
	if _, err := e2epod.WaitForPodsWithLabelRunningReady(fmw.ClientSet, fmw.Namespace.Name,
		labels.Set{"app": name}.AsSelector(), 1, 10*time.Second); err != nil {
		framework.DumpAllNamespaceInfo(fmw.ClientSet, fmw.Namespace.Name)
		kubectl.LogFailedContainers(fmw.ClientSet, fmw.Namespace.Name, framework.Logf)
		framework.Failf("unable to wait for all pods to be running and ready: %v", err)
	}
}