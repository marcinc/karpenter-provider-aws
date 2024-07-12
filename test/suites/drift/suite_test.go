/*
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

package drift_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/test"
	"github.com/aws/karpenter-provider-aws/test/pkg/environment/aws"
	"github.com/aws/karpenter-provider-aws/test/pkg/environment/common"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var env *aws.Environment
var amdAMI string
var nodeClass *v1.EC2NodeClass
var nodePool *karpv1.NodePool

func TestDrift(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		env = aws.NewEnvironment(t)
	})
	AfterSuite(func() {
		env.Stop()
	})
	RunSpecs(t, "Drift")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
	nodeClass = env.DefaultEC2NodeClass()
	nodePool = env.DefaultNodePool(nodeClass)
})
var _ = AfterEach(func() { env.Cleanup() })
var _ = AfterEach(func() { env.AfterEach() })

var _ = Describe("Drift", func() {
	var dep *appsv1.Deployment
	var selector labels.Selector
	var numPods int
	BeforeEach(func() {
		amdAMI = env.GetAMIBySSMPath(fmt.Sprintf("/aws/service/eks/optimized-ami/%s/amazon-linux-2023/x86_64/standard/recommended/image_id", env.K8sVersion()))
		numPods = 1
		// Add pods with a do-not-disrupt annotation so that we can check node metadata before we disrupt
		dep = coretest.Deployment(coretest.DeploymentOptions{
			Replicas: int32(numPods),
			PodOptions: coretest.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "my-app",
					},
					Annotations: map[string]string{
						karpv1.DoNotDisruptAnnotationKey: "true",
					},
				},
				TerminationGracePeriodSeconds: lo.ToPtr[int64](0),
			},
		})
		selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
	})
	Context("Budgets", func() {
		It("should respect budgets for empty drift", func() {
			nodePool = coretest.ReplaceRequirements(nodePool,
				karpv1.NodeSelectorRequirementWithMinValues{
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      v1.LabelInstanceSize,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"2xlarge"},
					},
				},
			)
			// We're expecting to create 3 nodes, so we'll expect to see 2 nodes deleting at one time.
			nodePool.Spec.Disruption.Budgets = []karpv1.Budget{{
				Nodes: "50%",
			}}
			var numPods int32 = 6
			dep = coretest.Deployment(coretest.DeploymentOptions{
				Replicas: numPods,
				PodOptions: coretest.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							karpv1.DoNotDisruptAnnotationKey: "true",
						},
						Labels: map[string]string{"app": "large-app"},
					},
					// Each 2xlarge has 8 cpu, so each node should fit 2 pods.
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			})
			selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
			env.ExpectCreated(nodeClass, nodePool, dep)

			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 3)
			nodes := env.EventuallyExpectCreatedNodeCount("==", 3)
			env.EventuallyExpectHealthyPodCount(selector, int(numPods))

			// List nodes so that we get any updated information on the nodes. If we don't
			// we have the potential to over-write any changes Karpenter makes to the nodes.
			// Add a finalizer to each node so that we can stop termination disruptions
			By("adding finalizers to the nodes to prevent termination")
			for _, node := range nodes {
				Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), node)).To(Succeed())
				node.Finalizers = append(node.Finalizers, common.TestingFinalizer)
				env.ExpectUpdated(node)
			}

			By("making the nodes empty")
			// Delete the deployment to make all nodes empty.
			env.ExpectDeleted(dep)

			// Drift the nodeclaims
			By("drift the nodeclaims")
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			env.EventuallyExpectDrifted(nodeClaims...)

			// Ensure that we get two nodes tainted, and they have overlap during the drift
			env.EventuallyExpectTaintedNodeCount("==", 2)
			nodes = env.ConsistentlyExpectDisruptionsWithNodeCount(2, 3, 5*time.Second)

			// Remove the finalizer from each node so that we can terminate
			for _, node := range nodes {
				Expect(env.ExpectTestingFinalizerRemoved(node)).To(Succeed())
			}

			// After the deletion timestamp is set and all pods are drained
			// the node should be gone
			env.EventuallyExpectNotFound(nodes[0], nodes[1])

			nodes = env.EventuallyExpectTaintedNodeCount("==", 1)
			Expect(env.ExpectTestingFinalizerRemoved(nodes[0])).To(Succeed())
			env.EventuallyExpectNotFound(nodes[0])
		})
		It("should respect budgets for non-empty delete drift", func() {
			nodePool = coretest.ReplaceRequirements(nodePool,
				karpv1.NodeSelectorRequirementWithMinValues{
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      v1.LabelInstanceSize,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"2xlarge"},
					},
				},
			)
			// We're expecting to create 3 nodes, so we'll expect to see at most 2 nodes deleting at one time.
			nodePool.Spec.Disruption.Budgets = []karpv1.Budget{{
				Nodes: "50%",
			}}
			var numPods int32 = 9
			dep = coretest.Deployment(coretest.DeploymentOptions{
				Replicas: numPods,
				PodOptions: coretest.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							karpv1.DoNotDisruptAnnotationKey: "true",
						},
						Labels: map[string]string{"app": "large-app"},
					},
					// Each 2xlarge has 8 cpu, so each node should fit no more than 3 pods.
					ResourceRequirements: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2100m"),
						},
					},
				},
			})
			selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
			env.ExpectCreated(nodeClass, nodePool, dep)

			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 3)
			nodes := env.EventuallyExpectCreatedNodeCount("==", 3)
			env.EventuallyExpectHealthyPodCount(selector, int(numPods))

			By("scaling down the deployment")
			// Update the deployment to a third of the replicas.
			dep.Spec.Replicas = lo.ToPtr[int32](3)
			env.ExpectUpdated(dep)

			// First expect there to be 3 pods, then try to spread the pods.
			env.EventuallyExpectHealthyPodCount(selector, 3)
			env.ForcePodsToSpread(nodes...)
			env.EventuallyExpectHealthyPodCount(selector, 3)

			By("cordoning and adding finalizer to the nodes")
			// Add a finalizer to each node so that we can stop termination disruptions
			for _, node := range nodes {
				Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), node)).To(Succeed())
				node.Finalizers = append(node.Finalizers, common.TestingFinalizer)
				env.ExpectUpdated(node)
			}

			By("drifting the nodes")
			// Drift the nodeclaims
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			env.EventuallyExpectDrifted(nodeClaims...)

			By("enabling disruption by removing the do not disrupt annotation")
			pods := env.EventuallyExpectHealthyPodCount(selector, 3)
			// Remove the do-not-disrupt annotation so that the nodes are now disruptable
			for _, pod := range pods {
				delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
				env.ExpectUpdated(pod)
			}

			// Ensure that we get two nodes tainted, and they have overlap during the drift
			env.EventuallyExpectTaintedNodeCount("==", 2)
			nodes = env.ConsistentlyExpectDisruptionsWithNodeCount(2, 3, 30*time.Second)

			By("removing the finalizer from the nodes")
			Expect(env.ExpectTestingFinalizerRemoved(nodes[0])).To(Succeed())
			Expect(env.ExpectTestingFinalizerRemoved(nodes[1])).To(Succeed())

			// After the deletion timestamp is set and all pods are drained
			// the node should be gone
			env.EventuallyExpectNotFound(nodes[0], nodes[1])
		})
		It("should respect budgets for non-empty replace drift", func() {
			appLabels := map[string]string{"app": "large-app"}
			nodePool.Labels = appLabels
			// We're expecting to create 5 nodes, so we'll expect to see at most 3 nodes deleting at one time.
			nodePool.Spec.Disruption.Budgets = []karpv1.Budget{{
				Nodes: "3",
			}}

			// Create a 5 pod deployment with hostname inter-pod anti-affinity to ensure each pod is placed on a unique node
			numPods = 5
			selector = labels.SelectorFromSet(appLabels)
			deployment := coretest.Deployment(coretest.DeploymentOptions{
				Replicas: int32(numPods),
				PodOptions: coretest.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: appLabels,
					},
					PodAntiRequirements: []corev1.PodAffinityTerm{{
						TopologyKey: corev1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: appLabels,
						},
					}},
				},
			})

			env.ExpectCreated(nodeClass, nodePool, deployment)

			originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", numPods)
			originalNodes := env.EventuallyExpectCreatedNodeCount("==", numPods)

			// Check that all deployment pods are online
			env.EventuallyExpectHealthyPodCount(selector, numPods)

			By("cordoning and adding finalizer to the nodes")
			// Add a finalizer to each node so that we can stop termination disruptions
			for _, node := range originalNodes {
				Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), node)).To(Succeed())
				node.Finalizers = append(node.Finalizers, common.TestingFinalizer)
				env.ExpectUpdated(node)
			}

			By("drifting the nodepool")
			nodePool.Spec.Template.Annotations = lo.Assign(nodePool.Spec.Template.Annotations, map[string]string{"test-annotation": "drift"})
			env.ExpectUpdated(nodePool)

			// Ensure that we get three nodes tainted, and they have overlap during the drift
			env.EventuallyExpectTaintedNodeCount("==", 3)
			env.EventuallyExpectNodeClaimCount("==", 8)
			env.EventuallyExpectNodeCount("==", 8)
			env.ConsistentlyExpectDisruptionsWithNodeCount(3, 8, 5*time.Second)

			for _, node := range originalNodes {
				Expect(env.ExpectTestingFinalizerRemoved(node)).To(Succeed())
			}

			// Eventually expect all the nodes to be rolled and completely removed
			// Since this completes the disruption operation, this also ensures that we aren't leaking nodes into subsequent
			// tests since nodeclaims that are actively replacing but haven't brought-up nodes yet can register nodes later
			env.EventuallyExpectNotFound(lo.Map(originalNodes, func(n *corev1.Node, _ int) client.Object { return n })...)
			env.EventuallyExpectNotFound(lo.Map(originalNodeClaims, func(n *karpv1.NodeClaim, _ int) client.Object { return n })...)
			env.ExpectNodeClaimCount("==", 5)
			env.ExpectNodeCount("==", 5)
		})
		It("should not allow drift if the budget is fully blocking", func() {
			// We're going to define a budget that doesn't allow any drift to happen
			nodePool.Spec.Disruption.Budgets = []karpv1.Budget{{
				Nodes: "0",
			}}

			dep.Spec.Template.Annotations = nil
			env.ExpectCreated(nodeClass, nodePool, dep)

			nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
			env.EventuallyExpectCreatedNodeCount("==", 1)
			env.EventuallyExpectHealthyPodCount(selector, numPods)

			By("drifting the nodes")
			// Drift the nodeclaims
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			env.EventuallyExpectDrifted(nodeClaim)
			env.ConsistentlyExpectNoDisruptions(1, time.Minute)
		})
		It("should not allow drift if the budget is fully blocking during a scheduled time", func() {
			// We're going to define a budget that doesn't allow any drift to happen
			// This is going to be on a schedule that only lasts 30 minutes, whose window starts 15 minutes before
			// the current time and extends 15 minutes past the current time
			// Times need to be in UTC since the karpenter containers were built in UTC time
			windowStart := time.Now().Add(-time.Minute * 15).UTC()
			nodePool.Spec.Disruption.Budgets = []karpv1.Budget{{
				Nodes:    "0",
				Schedule: lo.ToPtr(fmt.Sprintf("%d %d * * *", windowStart.Minute(), windowStart.Hour())),
				Duration: &metav1.Duration{Duration: time.Minute * 30},
			}}

			dep.Spec.Template.Annotations = nil
			env.ExpectCreated(nodeClass, nodePool, dep)

			nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
			env.EventuallyExpectCreatedNodeCount("==", 1)
			env.EventuallyExpectHealthyPodCount(selector, numPods)

			By("drifting the nodes")
			// Drift the nodeclaims
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			env.EventuallyExpectDrifted(nodeClaim)
			env.ConsistentlyExpectNoDisruptions(1, time.Minute)
		})
	})
	It("should disrupt nodes that have drifted due to AMIs", func() {
		// Choose an old static image (AL2023 AMIs don't exist for 1.22)
		oldCustomAMI := env.GetAMIBySSMPath(lo.Ternary(env.K8sMinorVersion() == 23,
			"/aws/service/eks/optimized-ami/1.23/amazon-linux-2023/x86_64/standard/amazon-eks-node-al2023-x86_64-standard-1.23-v20240307/image_id",
			fmt.Sprintf("/aws/service/eks/optimized-ami/%s/amazon-linux-2023/x86_64/standard/recommended/image_id", env.K8sVersionWithOffset(1)),
		))
		nodeClass.Spec.AMIFamily = &v1.AMIFamilyAL2023
		nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: oldCustomAMI}}

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		env.ExpectCreatedNodeCount("==", 1)

		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.EventuallyExpectNodeCount("==", 1)[0]
		nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: amdAMI}}
		env.ExpectCreatedOrUpdated(nodeClass)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, nodeClaim, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	})
	It("should return drifted if the AMI no longer matches the existing NodeClaims instance type", func() {
		armAMI := env.GetAMIBySSMPath(fmt.Sprintf("/aws/service/eks/optimized-ami/%s/amazon-linux-2023/arm64/standard/recommended/image_id", env.K8sVersion()))
		nodeClass.Spec.AMIFamily = &v1.AMIFamilyAL2023
		nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: armAMI}}

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		env.ExpectCreatedNodeCount("==", 1)

		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.EventuallyExpectNodeCount("==", 1)[0]
		nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: amdAMI}}
		env.ExpectCreatedOrUpdated(nodeClass)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, nodeClaim, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	})
	It("should not disrupt nodes that have drifted without the featureGate enabled", func() {
		env.ExpectSettingsOverridden(corev1.EnvVar{Name: "FEATURE_GATES", Value: "Drift=false"})

		// Choose an old static image (AL2023 AMIs don't exist for 1.22)
		oldCustomAMI := env.GetAMIBySSMPath(lo.Ternary(env.K8sMinorVersion() == 23,
			"/aws/service/eks/optimized-ami/1.23/amazon-linux-2023/x86_64/standard/amazon-eks-node-al2023-x86_64-standard-1.23-v20240307/image_id",
			fmt.Sprintf("/aws/service/eks/optimized-ami/%s/amazon-linux-2023/x86_64/standard/recommended/image_id", env.K8sVersionWithOffset(1)),
		))
		nodeClass.Spec.AMIFamily = &v1.AMIFamilyAL2023
		nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: oldCustomAMI}}

		env.ExpectCreated(dep, nodeClass, nodePool)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
		env.ExpectCreatedNodeCount("==", 1)

		node := env.Monitor.CreatedNodes()[0]
		nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: amdAMI}}
		env.ExpectUpdated(nodeClass)

		// We should consistently get the same node existing for a minute
		Consistently(func(g Gomega) {
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), &corev1.Node{})).To(Succeed())
		}).WithTimeout(time.Minute).Should(Succeed())
	})
	It("should disrupt nodes that have drifted due to securitygroup", func() {
		By("getting the cluster vpc id")
		output, err := env.EKSAPI.DescribeCluster(&eks.DescribeClusterInput{Name: awssdk.String(env.ClusterName)})
		Expect(err).To(BeNil())

		By("creating new security group")
		createSecurityGroup := &ec2.CreateSecurityGroupInput{
			GroupName:   awssdk.String("security-group-drift"),
			Description: awssdk.String("End-to-end Drift Test, should delete after drift test is completed"),
			VpcId:       output.Cluster.ResourcesVpcConfig.VpcId,
			TagSpecifications: []*ec2.TagSpecification{
				{
					ResourceType: awssdk.String("security-group"),
					Tags: []*ec2.Tag{
						{
							Key:   awssdk.String("karpenter.sh/discovery"),
							Value: awssdk.String(env.ClusterName),
						},
						{
							Key:   awssdk.String(coretest.DiscoveryLabel),
							Value: awssdk.String(env.ClusterName),
						},
						{
							Key:   awssdk.String("creation-date"),
							Value: awssdk.String(time.Now().Format(time.RFC3339)),
						},
					},
				},
			},
		}
		_, _ = env.EC2API.CreateSecurityGroup(createSecurityGroup)

		By("looking for security groups")
		var securitygroups []aws.SecurityGroup
		var testSecurityGroup aws.SecurityGroup
		Eventually(func(g Gomega) {
			securitygroups = env.GetSecurityGroups(map[string]string{"karpenter.sh/discovery": env.ClusterName})
			testSecurityGroup, _ = lo.Find(securitygroups, func(sg aws.SecurityGroup) bool {
				return awssdk.StringValue(sg.GroupName) == "security-group-drift"
			})
			g.Expect(testSecurityGroup).ToNot(BeNil())
		}).Should(Succeed())

		By("creating a new provider with the new securitygroup")
		awsIDs := lo.FilterMap(securitygroups, func(sg aws.SecurityGroup, _ int) (string, bool) {
			if awssdk.StringValue(sg.GroupId) != awssdk.StringValue(testSecurityGroup.GroupId) {
				return awssdk.StringValue(sg.GroupId), true
			}
			return "", false
		})
		sgTerms := []v1.SecurityGroupSelectorTerm{{ID: awssdk.StringValue(testSecurityGroup.GroupId)}}
		for _, id := range awsIDs {
			sgTerms = append(sgTerms, v1.SecurityGroupSelectorTerm{ID: id})
		}
		nodeClass.Spec.SecurityGroupSelectorTerms = sgTerms

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		sgTerms = lo.Reject(sgTerms, func(t v1.SecurityGroupSelectorTerm, _ int) bool {
			return t.ID == awssdk.StringValue(testSecurityGroup.GroupId)
		})
		nodeClass.Spec.SecurityGroupSelectorTerms = sgTerms
		env.ExpectCreatedOrUpdated(nodeClass)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, nodeClaim, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	})
	It("should disrupt nodes that have drifted due to subnets", func() {
		subnets := env.GetSubnetInfo(map[string]string{"karpenter.sh/discovery": env.ClusterName})
		Expect(len(subnets)).To(BeNumerically(">", 1))

		nodeClass.Spec.SubnetSelectorTerms = []v1.SubnetSelectorTerm{{ID: subnets[0].ID}}

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		nodeClass.Spec.SubnetSelectorTerms = []v1.SubnetSelectorTerm{{ID: subnets[1].ID}}
		env.ExpectCreatedOrUpdated(nodeClass)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	})
	DescribeTable("NodePool Drift", func(nodeClaimTemplate karpv1.NodeClaimTemplate) {
		updatedNodePool := coretest.NodePool(
			karpv1.NodePool{
				Spec: karpv1.NodePoolSpec{
					Template: karpv1.NodeClaimTemplate{
						Spec: karpv1.NodeClaimSpec{
							NodeClassRef: &karpv1.NodeClassReference{
								Group: object.GVK(nodeClass).Group,
								Kind:  object.GVK(nodeClass).Kind,
								Name:  nodeClass.Name,
							},
							// keep the same instance type requirements to prevent considering instance types that require swap
							Requirements: nodePool.Spec.Template.Spec.Requirements,
						},
					},
				},
			},
			karpv1.NodePool{
				Spec: karpv1.NodePoolSpec{
					Template: nodeClaimTemplate,
				},
			},
		)
		updatedNodePool.ObjectMeta = nodePool.ObjectMeta

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		env.ExpectCreatedOrUpdated(updatedNodePool)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)

		// Nodes will need to have the start-up taint removed before the node can be considered as initialized
		fmt.Println(CurrentSpecReport().LeafNodeText)
		if CurrentSpecReport().LeafNodeText == "Start-up Taints" {
			nodes := env.EventuallyExpectCreatedNodeCount("==", 2)
			sort.Slice(nodes, func(i int, j int) bool {
				return nodes[i].CreationTimestamp.Before(&nodes[j].CreationTimestamp)
			})
			nodeTwo := nodes[1]
			// Remove the startup taints from the new nodes to initialize them
			Eventually(func(g Gomega) {
				g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeTwo), nodeTwo)).To(Succeed())
				stored := nodeTwo.DeepCopy()
				nodeTwo.Spec.Taints = lo.Reject(nodeTwo.Spec.Taints, func(t corev1.Taint, _ int) bool { return t.Key == "example.com/another-taint-2" })
				g.Expect(env.Client.Patch(env.Context, nodeTwo, client.StrategicMergeFrom(stored))).To(Succeed())
			}).Should(Succeed())
		}
		env.EventuallyExpectNotFound(pod, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	},
		Entry("Annotations", karpv1.NodeClaimTemplate{
			ObjectMeta: karpv1.ObjectMeta{
				Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"},
			},
		}),
		Entry("Labels", karpv1.NodeClaimTemplate{
			ObjectMeta: karpv1.ObjectMeta{
				Labels: map[string]string{"keyLabelTest": "valueLabelTest"},
			},
		}),
		Entry("Taints", karpv1.NodeClaimTemplate{
			Spec: karpv1.NodeClaimSpec{
				Taints: []corev1.Taint{{Key: "example.com/another-taint-2", Effect: corev1.TaintEffectPreferNoSchedule}},
			},
		}),
		Entry("Start-up Taints", karpv1.NodeClaimTemplate{
			Spec: karpv1.NodeClaimSpec{
				StartupTaints: []corev1.Taint{{Key: "example.com/another-taint-2", Effect: corev1.TaintEffectPreferNoSchedule}},
			},
		}),
		Entry("NodeRequirements", karpv1.NodeClaimTemplate{
			Spec: karpv1.NodeClaimSpec{
				// since this will overwrite the default requirements, add instance category and family selectors back into requirements
				Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: karpv1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.CapacityTypeSpot}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.LabelInstanceCategory, Operator: corev1.NodeSelectorOpIn, Values: []string{"c", "m", "r"}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.LabelInstanceFamily, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"a1"}}},
				},
			},
		}),
	)
	DescribeTable("EC2NodeClass", func(nodeClassSpec v1.EC2NodeClassSpec) {
		updatedNodeClass := test.EC2NodeClass(v1.EC2NodeClass{Spec: *nodeClass.Spec.DeepCopy()}, v1.EC2NodeClass{Spec: nodeClassSpec})
		updatedNodeClass.ObjectMeta = nodeClass.ObjectMeta

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		env.ExpectCreatedOrUpdated(updatedNodeClass)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	},
		Entry("UserData", v1.EC2NodeClassSpec{UserData: awssdk.String("#!/bin/bash\necho \"Hello, AL2023\"")}),
		Entry("Tags", v1.EC2NodeClassSpec{Tags: map[string]string{"keyTag-test-3": "valueTag-test-3"}}),
		Entry("MetadataOptions", v1.EC2NodeClassSpec{MetadataOptions: &v1.MetadataOptions{HTTPTokens: awssdk.String("required"), HTTPPutResponseHopLimit: awssdk.Int64(10)}}),
		Entry("BlockDeviceMappings", v1.EC2NodeClassSpec{BlockDeviceMappings: []*v1.BlockDeviceMapping{
			{
				DeviceName: awssdk.String("/dev/xvda"),
				EBS: &v1.BlockDevice{
					VolumeSize: resources.Quantity("20Gi"),
					VolumeType: awssdk.String("gp3"),
					Encrypted:  awssdk.Bool(true),
				},
			}}}),
		Entry("DetailedMonitoring", v1.EC2NodeClassSpec{DetailedMonitoring: awssdk.Bool(true)}),
		Entry("AMIFamily", v1.EC2NodeClassSpec{AMIFamily: awssdk.String(v1.AMIFamilyBottlerocket)}),
		Entry("KubeletConfiguration", v1.EC2NodeClassSpec{
			Kubelet: &v1.KubeletConfiguration{
				EvictionSoft:            map[string]string{"memory.available": "5%"},
				EvictionSoftGracePeriod: map[string]metav1.Duration{"memory.available": {Duration: time.Minute}},
			},
		}),
	)
	It("should drift the EC2NodeClass on InstanceProfile", func() {
		// Create a separate test case for this one since we can't use the default NodeClass that's created due to it having
		// a pre-populated role AND we also need to do the instance profile generation within the scope of this test
		instanceProfileName := fmt.Sprintf("KarpenterNodeInstanceProfile-%s", env.ClusterName)
		instanceProfileDriftName := fmt.Sprintf("KarpenterNodeInstanceProfile-Drift-%s", env.ClusterName)
		roleName := fmt.Sprintf("KarpenterNodeRole-%s", env.ClusterName)

		for _, name := range []string{instanceProfileName, instanceProfileDriftName} {
			env.ExpectInstanceProfileCreated(name, roleName)
			DeferCleanup(func() {
				env.ExpectInstanceProfileDeleted(name, roleName)
			})
		}
		nodeClass.Spec.Role = ""
		nodeClass.Spec.InstanceProfile = lo.ToPtr(instanceProfileName)

		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		nodeClass.Spec.InstanceProfile = lo.ToPtr(instanceProfileDriftName)
		env.ExpectCreatedOrUpdated(nodeClass)

		env.EventuallyExpectDrifted(nodeClaim)

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	})
	It("should drift the EC2NodeClass on BlockDeviceMappings volume size update", func() {
		nodeClass.Spec.BlockDeviceMappings = []*v1.BlockDeviceMapping{
			{
				DeviceName: awssdk.String("/dev/xvda"),
				EBS: &v1.BlockDevice{
					VolumeSize: resources.Quantity("20Gi"),
					VolumeType: awssdk.String("gp3"),
					Encrypted:  awssdk.Bool(true),
				},
			},
		}
		env.ExpectCreated(dep, nodeClass, nodePool)
		pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		node := env.ExpectCreatedNodeCount("==", 1)[0]

		nodeClass.Spec.BlockDeviceMappings[0].EBS.VolumeSize = resources.Quantity("100Gi")
		env.ExpectCreatedOrUpdated(nodeClass)

		By("validating the drifted status condition has propagated")
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())
			g.Expect(nodeClaim.StatusConditions().Get(karpv1.ConditionTypeDrifted)).ToNot(BeNil())
			g.Expect(nodeClaim.StatusConditions().Get(karpv1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		}).Should(Succeed())

		delete(pod.Annotations, karpv1.DoNotDisruptAnnotationKey)
		env.ExpectUpdated(pod)
		env.EventuallyExpectNotFound(pod, node)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
	})
	It("should update the nodepool-hash annotation on the nodepool and nodeclaim when the nodepool's nodepool-hash-version annotation does not match the controller hash version", func() {
		env.ExpectCreated(dep, nodeClass, nodePool)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		nodePool = env.ExpectExists(nodePool).(*karpv1.NodePool)
		expectedHash := nodePool.Hash()

		By(fmt.Sprintf("expect nodepool %s and nodeclaim %s to contain %s and %s annotations", nodePool.Name, nodeClaim.Name, karpv1.NodePoolHashAnnotationKey, karpv1.NodePoolHashVersionAnnotationKey))
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodePool), nodePool)).To(Succeed())
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())

			g.Expect(nodePool.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashAnnotationKey, expectedHash))
			g.Expect(nodePool.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashVersionAnnotationKey, karpv1.NodePoolHashVersion))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashAnnotationKey, expectedHash))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashVersionAnnotationKey, karpv1.NodePoolHashVersion))
		}).WithTimeout(30 * time.Second).Should(Succeed())

		nodePool.Annotations = lo.Assign(nodePool.Annotations, map[string]string{
			karpv1.NodePoolHashAnnotationKey:        "test-hash-1",
			karpv1.NodePoolHashVersionAnnotationKey: "test-hash-version-1",
		})
		// Updating `nodePool.Spec.Template.Annotations` would normally trigger drift on all nodeclaims owned by the
		// nodepool. However, the nodepool-hash-version does not match the controller hash version, so we will see that
		// none of the nodeclaims will be drifted and all nodeclaims will have an updated `nodepool-hash` and `nodepool-hash-version` annotation
		nodePool.Spec.Template.Annotations = lo.Assign(nodePool.Spec.Template.Annotations, map[string]string{
			"test-key": "test-value",
		})
		nodeClaim.Annotations = lo.Assign(nodePool.Annotations, map[string]string{
			karpv1.NodePoolHashAnnotationKey:        "test-hash-2",
			karpv1.NodePoolHashVersionAnnotationKey: "test-hash-version-2",
		})

		// The nodeclaim will need to be updated first, as the hash controller will only be triggered on changes to the nodepool
		env.ExpectUpdated(nodeClaim, nodePool)
		expectedHash = nodePool.Hash()

		// Expect all nodeclaims not to be drifted and contain an updated `nodepool-hash` and `nodepool-hash-version` annotation
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodePool), nodePool)).To(Succeed())
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())

			g.Expect(nodePool.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashAnnotationKey, expectedHash))
			g.Expect(nodePool.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashVersionAnnotationKey, karpv1.NodePoolHashVersion))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashAnnotationKey, expectedHash))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(karpv1.NodePoolHashVersionAnnotationKey, karpv1.NodePoolHashVersion))
		})
	})
	It("should update the ec2nodeclass-hash annotation on the ec2nodeclass and nodeclaim when the ec2nodeclass's ec2nodeclass-hash-version annotation does not match the controller hash version", func() {
		env.ExpectCreated(dep, nodeClass, nodePool)
		env.EventuallyExpectHealthyPodCount(selector, numPods)
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		nodeClass = env.ExpectExists(nodeClass).(*v1.EC2NodeClass)
		expectedHash := nodeClass.Hash()

		By(fmt.Sprintf("expect nodeclass %s and nodeclaim %s to contain %s and %s annotations", nodeClass.Name, nodeClaim.Name, v1.AnnotationEC2NodeClassHash, v1.AnnotationEC2NodeClassHashVersion))
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClass), nodeClass)).To(Succeed())
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())

			g.Expect(nodeClass.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHash, expectedHash))
			g.Expect(nodeClass.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHashVersion, v1.EC2NodeClassHashVersion))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHash, expectedHash))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHashVersion, v1.EC2NodeClassHashVersion))
		}).WithTimeout(30 * time.Second).Should(Succeed())

		nodeClass.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{
			v1.AnnotationEC2NodeClassHash:        "test-hash-1",
			v1.AnnotationEC2NodeClassHashVersion: "test-hash-version-1",
		})
		// Updating `nodeClass.Spec.Tags` would normally trigger drift on all nodeclaims using the
		// nodeclass. However, the ec2nodeclass-hash-version does not match the controller hash version, so we will see that
		// none of the nodeclaims will be drifted and all nodeclaims will have an updated `ec2nodeclass-hash` and `ec2nodeclass-hash-version` annotation
		nodeClass.Spec.Tags = lo.Assign(nodeClass.Spec.Tags, map[string]string{
			"test-key": "test-value",
		})
		nodeClaim.Annotations = lo.Assign(nodePool.Annotations, map[string]string{
			v1.AnnotationEC2NodeClassHash:        "test-hash-2",
			v1.AnnotationEC2NodeClassHashVersion: "test-hash-version-2",
		})

		// The nodeclaim will need to be updated first, as the hash controller will only be triggered on changes to the nodeclass
		env.ExpectUpdated(nodeClaim, nodeClass)
		expectedHash = nodeClass.Hash()

		// Expect all nodeclaims not to be drifted and contain an updated `nodepool-hash` and `nodepool-hash-version` annotation
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClass), nodeClass)).To(Succeed())
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(nodeClaim), nodeClaim)).To(Succeed())

			g.Expect(nodeClass.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHash, expectedHash))
			g.Expect(nodeClass.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHashVersion, v1.EC2NodeClassHashVersion))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHash, expectedHash))
			g.Expect(nodeClaim.Annotations).To(HaveKeyWithValue(v1.AnnotationEC2NodeClassHashVersion, v1.EC2NodeClassHashVersion))
		}).WithTimeout(30 * time.Second).Should(Succeed())
		env.ConsistentlyExpectNodeClaimsNotDrifted(time.Minute, nodeClaim)
	})
	Context("Failure", func() {
		It("should not disrupt a drifted node if the replacement node never registers", func() {
			// launch a new nodeClaim
			var numPods int32 = 2
			dep := coretest.Deployment(coretest.DeploymentOptions{
				Replicas: 2,
				PodOptions: coretest.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "inflate"}},
					PodAntiRequirements: []corev1.PodAffinityTerm{{
						TopologyKey: corev1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "inflate"},
						}},
					},
				},
			})
			env.ExpectCreated(dep, nodeClass, nodePool)

			startingNodeClaimState := env.EventuallyExpectCreatedNodeClaimCount("==", int(numPods))
			env.EventuallyExpectCreatedNodeCount("==", int(numPods))

			// Drift the nodeClaim with bad configuration that will not register a NodeClaim
			nodeClass.Spec.AMISelectorTerms = []v1.AMISelectorTerm{{ID: env.GetAMIBySSMPath("/aws/service/ami-amazon-linux-latest/amzn2-ami-hvm-x86_64-ebs")}}
			env.ExpectCreatedOrUpdated(nodeClass)

			env.EventuallyExpectDrifted(startingNodeClaimState...)

			// Expect only a single node to be tainted due to default disruption budgets
			taintedNodes := env.EventuallyExpectTaintedNodeCount("==", 1)

			// Drift should fail and the original node should be untainted
			// TODO: reduce timeouts when disruption waits are factored out
			env.EventuallyExpectNodesUntaintedWithTimeout(11*time.Minute, taintedNodes...)

			// Expect all the NodeClaims that existed on the initial provisioning loop are not removed.
			// Assert this over several minutes to ensure a subsequent disruption controller pass doesn't
			// successfully schedule the evicted pods to the in-flight nodeclaim and disrupt the original node
			Consistently(func(g Gomega) {
				nodeClaims := &karpv1.NodeClaimList{}
				g.Expect(env.Client.List(env, nodeClaims, client.HasLabels{coretest.DiscoveryLabel})).To(Succeed())
				startingNodeClaimUIDs := sets.New(lo.Map(startingNodeClaimState, func(nc *karpv1.NodeClaim, _ int) types.UID { return nc.UID })...)
				nodeClaimUIDs := sets.New(lo.Map(nodeClaims.Items, func(nc karpv1.NodeClaim, _ int) types.UID { return nc.UID })...)
				g.Expect(nodeClaimUIDs.IsSuperset(startingNodeClaimUIDs)).To(BeTrue())
			}, "2m").Should(Succeed())
		})
		It("should not disrupt a drifted node if the replacement node registers but never initialized", func() {
			// launch a new nodeClaim
			var numPods int32 = 2
			dep := coretest.Deployment(coretest.DeploymentOptions{
				Replicas: 2,
				PodOptions: coretest.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "inflate"}},
					PodAntiRequirements: []corev1.PodAffinityTerm{{
						TopologyKey: corev1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "inflate"},
						}},
					},
				},
			})
			env.ExpectCreated(dep, nodeClass, nodePool)

			startingNodeClaimState := env.EventuallyExpectCreatedNodeClaimCount("==", int(numPods))
			env.EventuallyExpectCreatedNodeCount("==", int(numPods))

			// Drift the nodeClaim with bad configuration that never initializes
			nodePool.Spec.Template.Spec.StartupTaints = []corev1.Taint{{Key: "example.com/taint", Effect: corev1.TaintEffectPreferNoSchedule}}
			env.ExpectCreatedOrUpdated(nodePool)

			env.EventuallyExpectDrifted(startingNodeClaimState...)

			// Expect only a single node to get tainted due to default disruption budgets
			taintedNodes := env.EventuallyExpectTaintedNodeCount("==", 1)

			// Drift should fail and original node should be untainted
			// TODO: reduce timeouts when disruption waits are factored out
			env.EventuallyExpectNodesUntaintedWithTimeout(11*time.Minute, taintedNodes...)

			// Expect that the new nodeClaim/node is kept around after the un-cordon
			nodeList := &corev1.NodeList{}
			Expect(env.Client.List(env, nodeList, client.HasLabels{coretest.DiscoveryLabel})).To(Succeed())
			Expect(nodeList.Items).To(HaveLen(int(numPods) + 1))

			nodeClaimList := &karpv1.NodeClaimList{}
			Expect(env.Client.List(env, nodeClaimList, client.HasLabels{coretest.DiscoveryLabel})).To(Succeed())
			Expect(nodeClaimList.Items).To(HaveLen(int(numPods) + 1))

			// Expect all the NodeClaims that existed on the initial provisioning loop are not removed
			// Assert this over several minutes to ensure a subsequent disruption controller pass doesn't
			// successfully schedule the evicted pods to the in-flight nodeclaim and disrupt the original node
			Consistently(func(g Gomega) {
				nodeClaims := &karpv1.NodeClaimList{}
				g.Expect(env.Client.List(env, nodeClaims, client.HasLabels{coretest.DiscoveryLabel})).To(Succeed())
				startingNodeClaimUIDs := sets.New(lo.Map(startingNodeClaimState, func(m *karpv1.NodeClaim, _ int) types.UID { return m.UID })...)
				nodeClaimUIDs := sets.New(lo.Map(nodeClaims.Items, func(m karpv1.NodeClaim, _ int) types.UID { return m.UID })...)
				g.Expect(nodeClaimUIDs.IsSuperset(startingNodeClaimUIDs)).To(BeTrue())
			}, "2m").Should(Succeed())
		})
		It("should not drift any nodes if their PodDisruptionBudgets are unhealthy", func() {
			// Create a deployment that contains a readiness probe that will never succeed
			// This way, the pod will bind to the node, but the PodDisruptionBudget will never go healthy
			var numPods int32 = 2
			dep := coretest.Deployment(coretest.DeploymentOptions{
				Replicas: 2,
				PodOptions: coretest.PodOptions{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "inflate"}},
					PodAntiRequirements: []corev1.PodAffinityTerm{{
						TopologyKey: corev1.LabelHostname,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "inflate"},
						}},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Port: intstr.FromInt32(80),
							},
						},
					},
				},
			})
			selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
			minAvailable := intstr.FromInt32(numPods - 1)
			pdb := coretest.PodDisruptionBudget(coretest.PDBOptions{
				Labels:       dep.Spec.Template.Labels,
				MinAvailable: &minAvailable,
			})
			env.ExpectCreated(dep, nodeClass, nodePool, pdb)

			nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", int(numPods))
			env.EventuallyExpectCreatedNodeCount("==", int(numPods))

			// Expect pods to be bound but not to be ready since we are intentionally failing the readiness check
			env.EventuallyExpectBoundPodCount(selector, int(numPods))

			// Drift the nodeclaims
			nodePool.Spec.Template.Annotations = map[string]string{"test": "annotation"}
			env.ExpectUpdated(nodePool)

			env.EventuallyExpectDrifted(nodeClaims...)
			env.ConsistentlyExpectNoDisruptions(int(numPods), time.Minute)
		})
	})
})
