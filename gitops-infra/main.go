package main

import (
	b64 "encoding/base64"
	"fmt"
	v12 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/apps/v1"
	rbac "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"

	"github.com/pulumi/pulumi-aws/sdk/v5/go/aws/ec2"
	"github.com/pulumi/pulumi-eks/sdk/go/eks"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/apiextensions"
	v1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/helm/v3"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

var (
	publicSubnetCidrs = []string{
		"10.0.0.0/27",
		"10.0.0.32/27",
	}
	availabilityZones = []string{
		"eu-central-1a",
		"eu-central-1b",
	}
)

const (
	clusterName = "pulumi-backstage-flux-gitops-aws"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		vpc, err := ec2.NewVpc(ctx, "pulumi-backstage-flux-gitops-aws-vpc", &ec2.VpcArgs{
			CidrBlock: pulumi.String("10.0.0.0/24"),
		})
		if err != nil {
			return err
		}
		ctx.Export("vpc-id", vpc.ID())

		igw, err := ec2.NewInternetGateway(ctx, "pulumi-backstage-flux-gitops-aws-igw", &ec2.InternetGatewayArgs{
			VpcId: vpc.ID(),
		})
		if err != nil {
			return err
		}

		rt, err := ec2.NewRouteTable(ctx, "pulumi-backstage-flux-gitops-aws-rt", &ec2.RouteTableArgs{
			VpcId: vpc.ID(),
			Routes: ec2.RouteTableRouteArray{
				&ec2.RouteTableRouteArgs{
					CidrBlock: pulumi.String("0.0.0.0/0"),
					GatewayId: igw.ID(),
				},
			},
		})
		if err != nil {
			return err
		}
		ctx.Export("route-table-id", rt.ID())

		var publicSubnetIDs pulumi.StringArray

		// Create a subnet for each availability zone
		for i, az := range availabilityZones {
			publicSubnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("pulumi-backstage-flux-gitops-aws-subnet-%d", i), &ec2.SubnetArgs{
				VpcId:                       vpc.ID(),
				CidrBlock:                   pulumi.String(publicSubnetCidrs[i]),
				MapPublicIpOnLaunch:         pulumi.Bool(false),
				AssignIpv6AddressOnCreation: pulumi.Bool(false),
				AvailabilityZone:            pulumi.String(az),
				Tags: pulumi.StringMap{
					"Name": pulumi.Sprintf("pulumi-backstage-flux-gitops-aws-subnet-%d", az),
				},
			})
			if err != nil {
				return err
			}
			_, err = ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("pulumi-backstage-flux-gitops-aws-rt-association-%s", az), &ec2.RouteTableAssociationArgs{
				RouteTableId: rt.ID(),
				SubnetId:     publicSubnet.ID(),
			})
			if err != nil {
				return err
			}
			publicSubnetIDs = append(publicSubnetIDs, publicSubnet.ID())
		}
		ctx.Export("public-subnet-ids", publicSubnetIDs)

		cluster, err := eks.NewCluster(ctx, clusterName, &eks.ClusterArgs{
			Name:                 pulumi.String(clusterName),
			VpcId:                vpc.ID(),
			PrivateSubnetIds:     publicSubnetIDs,
			EndpointPublicAccess: pulumi.Bool(true),
			InstanceType:         pulumi.String("t3.medium"),
			DesiredCapacity:      pulumi.Int(2),
			MinSize:              pulumi.Int(1),
			MaxSize:              pulumi.Int(3),
			ProviderCredentialOpts: eks.KubeconfigOptionsArgs{
				ProfileName: pulumi.String("default"),
			},
			CreateOidcProvider: pulumi.Bool(true),
		})
		if err != nil {
			return err
		}

		ctx.Export("kubeconfig", pulumi.ToSecret(cluster.Kubeconfig))

		k8sProvider, err := kubernetes.NewProvider(ctx, "kubernetes-provider", &kubernetes.ProviderArgs{
			Kubeconfig:            cluster.KubeconfigJson,
			EnableServerSideApply: pulumi.Bool(true),
		}, pulumi.DependsOn([]pulumi.Resource{cluster}))
		if err != nil {
			return err
		}

		backStageLabel := pulumi.StringMap{
			"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
		}

		flux, err := helm.NewRelease(ctx, "pulumi-backstage-flux-gitops-aws-flux2", &helm.ReleaseArgs{
			Chart:           pulumi.String("oci://ghcr.io/fluxcd-community/charts/flux2"),
			Namespace:       pulumi.String("flux-system"),
			CreateNamespace: pulumi.Bool(true),
			Version:         pulumi.String("2.10.0"),
			Values: pulumi.Map{
				"helmController": pulumi.Map{
					"labels": backStageLabel,
				},
				"kustomizeController": pulumi.Map{
					"labels": backStageLabel,
				},
				"notificationController": pulumi.Map{
					"labels": backStageLabel,
				},
				"sourceController": pulumi.Map{
					"labels": backStageLabel,
				},
				"imageReflectionController": pulumi.Map{
					"labels": backStageLabel,
				},
				"imageAutomationController": pulumi.Map{
					"labels": backStageLabel,
				},
			},
		}, pulumi.Provider(k8sProvider))
		if err != nil {
			return err
		}

		for _, controller := range []string{"kustomize-controller", "helm-controller", "notification-controller", "source-controller", "image-reflector-controller", "image-automation-controller"} {
			_, err = v12.NewDeploymentPatch(ctx, fmt.Sprintf("pulumi-backstage-flux-gitops-aws-%s-patch", controller), &v12.DeploymentPatchArgs{
				Metadata: &metav1.ObjectMetaPatchArgs{
					Annotations: pulumi.StringMap{
						"pulumi.com/patchForce": pulumi.String("true"),
					},
					Name:      pulumi.String(controller),
					Namespace: flux.Namespace,
					Labels: pulumi.StringMap{
						"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
					},
				},
			}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{flux}))
			if err != nil {
				return err
			}
		}

		pulumiOCIRepo, err := apiextensions.NewCustomResource(ctx, "pulumi-backstage-flux-gitops-aws-pulumi-repo", &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("source.toolkit.fluxcd.io/v1beta2"),
			Kind:       pulumi.String("HelmRepository"),
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("pulumi-oci-repo"),
				Namespace: flux.Namespace,
				Labels: pulumi.StringMap{
					"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
				},
			},
			OtherFields: kubernetes.UntypedArgs{
				"spec": kubernetes.UntypedArgs{
					"interval": pulumi.String("1m"),
					"type":     pulumi.String("oci"),
					"url":      pulumi.String("oci://ghcr.io/pulumi/helm-charts"),
				},
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{flux}))
		if err != nil {
			return err
		}

		_, err = apiextensions.NewCustomResource(ctx, "pulumi-backstage-flux-gitops-aws-pulumi-operator-release", &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("helm.toolkit.fluxcd.io/v2beta1"),
			Kind:       pulumi.String("HelmRelease"),
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("pulumi-operator-helm-release"),
				Namespace: pulumiOCIRepo.Metadata.Namespace(),
				Labels: pulumi.StringMap{
					"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
				},
			},
			OtherFields: kubernetes.UntypedArgs{
				"spec": pulumi.Map{
					"interval": pulumi.String("1m"),
					"install": pulumi.Map{
						"createNamespace": pulumi.Bool(true),
						"crds":            pulumi.String("CreateReplace"),
					},
					"targetNamespace": pulumi.String("pulumi-operator"),
					"chart": pulumi.Map{
						"spec": pulumi.Map{
							"chart":    pulumi.String("pulumi-kubernetes-operator"),
							"interval": pulumi.String("1m"),
							"version":  pulumi.String("0.2.0"),
							"sourceRef": pulumi.Map{
								"kind":      pulumiOCIRepo.Kind,
								"name":      pulumiOCIRepo.Metadata.Name(),
								"namespace": pulumiOCIRepo.Metadata.Namespace(),
							},
						},
					},
					"postRenderers": pulumi.Array{
						pulumi.Map{
							"kustomize": pulumi.Map{
								"patchesStrategicMerge": pulumi.Array{
									pulumi.Map{
										"kind":       pulumi.String("Deployment"),
										"apiVersion": pulumi.String("apps/v1"),
										"metadata": pulumi.Map{
											"name": pulumi.String("pulumi-operator"),
											"labels": pulumi.StringMap{
												"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
											},
										},
										"spec": pulumi.Map{
											"template": pulumi.Map{
												"metadata": pulumi.Map{
													"labels": pulumi.StringMap{
														"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
													},
												},
											},
											"selector": pulumi.Map{
												"matchLabels": pulumi.StringMap{
													"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
												},
											},
										},
									},
								},
								/*
									"patchesJson6902": pulumi.Array{
										pulumi.Map{
											"target": pulumi.Map{
												"version": pulumi.String("v1"),
												"kind":    pulumi.String("Deployment"),
												"name":    pulumi.String("pulumi-operator"),
											},
											"patch": pulumi.Array{
												pulumi.Map{
													"op":    pulumi.String("add"),
													"path":  pulumi.String("/metadata/labels"),
													"value": pulumi.StringMap{"backstage.io/kubernetes-id": pulumi.String("gitops-cluster")},
												},
												pulumi.Map{
													"op":    pulumi.String("add"),
													"path":  pulumi.String("/spec/template/metadata/labels/backstage.io~1kubernetes-id"),
													"value": pulumi.String("gitops-cluster"),
												},
												pulumi.Map{
													"op":    pulumi.String("add"),
													"path":  pulumi.String("/spec/selector/matchLabels/backstage.io~1kubernetes-id"),
													"value": pulumi.String("gitops-cluster"),
												},
											},
										},
									},*/
							},
						},
					},
					"values": pulumi.Map{
						"extraEnv": pulumi.Array{
							pulumi.Map{
								"name":  pulumi.String("PULUMI_ACCESS_TOKEN"),
								"value": config.GetSecret(ctx, "pulumi-pat"),
							},
						},
						"fullnameOverride": pulumi.String("pulumi-operator"),
					},
				},
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{pulumiOCIRepo}))
		if err != nil {
			return err
		}

		gitRepo, err := apiextensions.NewCustomResource(ctx, "pulumi-backstage-flux-gitops-aws-repo", &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("source.toolkit.fluxcd.io/v1"),
			Kind:       pulumi.String("GitRepository"),
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("pulumi-backstage-flux-gitops-aws-git-repo"),
				Namespace: flux.Namespace.Elem(),
				Labels: pulumi.StringMap{
					"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
				},
			},
			OtherFields: kubernetes.UntypedArgs{
				"spec": pulumi.Map{
					"url": pulumi.String("https://github.com/my-backstage-demo/pulumi-infrastructure"),
					"ref": pulumi.Map{
						"branch": pulumi.String("main"),
					},
					"interval": pulumi.String("1m"),
				},
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{flux}))
		if err != nil {
			return err
		}

		_, err = apiextensions.NewCustomResource(ctx, "pulumi-backstage-flux-gitops-aws-kustomization", &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("kustomize.toolkit.fluxcd.io/v1"),
			Kind:       pulumi.String("Kustomization"),
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("pulumi-backstage-flux-gitops-aws-kustomization"),
				Namespace: pulumi.String("pulumi-operator"),
				Labels: pulumi.StringMap{
					"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
				},
			},
			OtherFields: kubernetes.UntypedArgs{
				"spec": pulumi.Map{
					"interval":        pulumi.String("1m"),
					"prune":           pulumi.Bool(true),
					"force":           pulumi.Bool(false),
					"targetNamespace": pulumi.String("pulumi-operator"),
					"sourceRef": pulumi.Map{
						"kind":      gitRepo.Kind,
						"name":      gitRepo.Metadata.Name(),
						"namespace": gitRepo.Metadata.Namespace(),
					},
					"path": pulumi.String("./kustomize"),
				},
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{gitRepo}))
		if err != nil {
			return err
		}

		// get ready for backstage by creating a sa with cluster-admin role
		backstageSA, err := v1.NewServiceAccount(ctx, "pulumi-backstage-flux-gitops-aws-backstage-sa", &v1.ServiceAccountArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name: pulumi.String("backstage"),
			},
		}, pulumi.Provider(k8sProvider))

		_, err = rbac.NewClusterRoleBinding(ctx, "pulumi-backstage-flux-gitops-aws-backstage-cluster-role-binding", &rbac.ClusterRoleBindingArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name: pulumi.String("backstage-cluster-role-binding"),
			},
			RoleRef: &rbac.RoleRefArgs{
				ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
				Kind:     pulumi.String("ClusterRole"),
				Name:     pulumi.String("cluster-admin"),
			},
			Subjects: rbac.SubjectArray{
				&rbac.SubjectArgs{
					Kind:      pulumi.String("ServiceAccount"),
					Name:      backstageSA.Metadata.Name().Elem(),
					Namespace: backstageSA.Metadata.Namespace().Elem(),
				},
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{backstageSA}))
		if err != nil {
			return err
		}

		backstageToken, err := v1.NewSecret(ctx, "pulumi-backstage-flux-gitops-aws-backstage-token", &v1.SecretArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name: pulumi.String("backstage-token"),
				Annotations: pulumi.StringMap{
					"kubernetes.io/service-account.name": backstageSA.Metadata.Name().Elem(),
				},
			},
			Type: pulumi.String("kubernetes.io/service-account-token"),
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{backstageSA}))
		if err != nil {
			return err
		}

		ctx.Export("backstage-token", backstageToken.Data.ApplyT(func(data map[string]string) string {
			token, _ := b64.StdEncoding.DecodeString(data["token"])
			return string(token)
		}).(pulumi.StringOutput))

		ctx.Export("gitops-platform-endpoint", cluster.EksCluster.Endpoint())

		return nil
	})
}
