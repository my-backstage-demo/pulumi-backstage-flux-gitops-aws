package main

import (
	b64 "encoding/base64"
	"fmt"
	"github.com/pulumi/pulumi-aws/sdk/v5/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v5/go/aws/iam"
	"github.com/pulumi/pulumi-eks/sdk/go/eks"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/apiextensions"
	v1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/helm/v3"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/meta/v1"
	rbac "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
	"os"
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
	clusterName       = "pulumi-backstage-flux-gitops-aws"
	albNamespace      = "aws-lb-controller"
	albServiceAccount = "system:serviceaccount:" + albNamespace + ":aws-lb-controller-serviceaccount"
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
			Version:            pulumi.String(config.Get(ctx, "eksVersion")),
			CreateOidcProvider: pulumi.Bool(true),
		})
		if err != nil {
			return err
		}

		ctx.Export("kubeconfig", pulumi.ToSecret(cluster.Kubeconfig))

		// enable ALB
		albRole, err := iam.NewRole(ctx, "alb-role", &iam.RoleArgs{
			AssumeRolePolicy: pulumi.All(cluster.Core.OidcProvider().Arn(), cluster.Core.OidcProvider().Url()).ApplyT(func(args []interface{}) string {
				arn := args[0].(string)
				url := args[1].(string)
				assumeRolePolicy, _ := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
					Statements: []iam.GetPolicyDocumentStatement{
						{
							Effect: pulumi.StringRef("Allow"),
							Actions: []string{
								"sts:AssumeRoleWithWebIdentity",
							},
							Principals: []iam.GetPolicyDocumentStatementPrincipal{
								{
									Type: "Federated",
									Identifiers: []string{
										arn,
									},
								},
							},
							Conditions: []iam.GetPolicyDocumentStatementCondition{
								{
									Test: "StringEquals",
									Values: []string{
										albServiceAccount,
									},
									Variable: fmt.Sprintf("%s:sub", url),
								},
							},
						},
					},
				})
				return assumeRolePolicy.Json
			}).(pulumi.StringOutput),
		})
		if err != nil {
			return err
		}

		albPolicyFile, err := os.ReadFile("./iam-policies/alb-iam-policy.json")
		if err != nil {
			return err
		}

		albIAMPolicy, err := iam.NewPolicy(ctx, "alb-policy", &iam.PolicyArgs{
			Policy: pulumi.String(albPolicyFile),
		}, pulumi.DependsOn([]pulumi.Resource{albRole}))
		if err != nil {
			return err
		}

		_, err = iam.NewRolePolicyAttachment(ctx, "alb-role-attachment", &iam.RolePolicyAttachmentArgs{
			PolicyArn: albIAMPolicy.Arn,
			Role:      albRole.Name,
		}, pulumi.DependsOn([]pulumi.Resource{albIAMPolicy}))
		if err != nil {
			return err
		}

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
			Version:         pulumi.String("2.11.1"),
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

		_, err = v1.NewSecret(ctx, "aws-lb-controller-secret", &v1.SecretArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("aws-load-balancer-controller-values"),
				Namespace: flux.Namespace,
			},
			StringData: pulumi.StringMap{
				"values.yaml": pulumi.Sprintf(`clusterName: %s
region: eu-central-1
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: %s
vpcId: %s`, cluster.EksCluster.Name(), albRole.Arn, vpc.ID()),
			},
		}, pulumi.Provider(k8sProvider))
		if err != nil {
			return err
		}

		// create namespace for the Pulumi Operator
		operatorNS, err := v1.NewNamespace(ctx, "pulumi-backstage-flux-gitops-aws-pulumi-operator-ns", &v1.NamespaceArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name: pulumi.String("pulumi-operator"),
			},
		}, pulumi.Provider(k8sProvider))
		if err != nil {
			return err
		}

		// add secret with Pulumi access token
		_, err = v1.NewSecret(ctx, "pulumi-backstage-flux-gitops-aws-pulumi-access-token", &v1.SecretArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:      pulumi.String("pulumi-access-token"),
				Namespace: operatorNS.Metadata.Name(),
			},
			Type: pulumi.String("Opaque"),
			StringData: pulumi.StringMap{
				"pulumi-access-token": config.GetSecret(ctx, "pulumi-pat"),
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{operatorNS}))

		// deploy boostrap repository Kustomization
		boostrapRepo, err := apiextensions.NewCustomResource(ctx, "pulumi-backstage-flux-gitops-aws-bootstrap-repo", &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("source.toolkit.fluxcd.io/v1"),
			Kind:       pulumi.String("GitRepository"),
			Metadata: &metav1.ObjectMetaArgs{
				Name: pulumi.String("bootstrap-repo"),
				Labels: pulumi.StringMap{
					"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
				},
				Namespace: flux.Namespace,
			},
			OtherFields: kubernetes.UntypedArgs{
				"spec": pulumi.Map{
					"interval": pulumi.String("1m"),
					"ref": pulumi.Map{
						"branch": pulumi.String("main"),
					},
					"timeout": pulumi.String("60s"),
					"url":     pulumi.String("https://github.com/my-backstage-demo/pulumi-gitops-repo.git"),
				},
			},
		}, pulumi.Provider(k8sProvider), pulumi.DependsOn([]pulumi.Resource{flux}))
		if err != nil {
			return err
		}
		_, err = apiextensions.NewCustomResource(ctx, "pulumi-backstage-flux-gitops-aws-bootstrap-kustomization", &apiextensions.CustomResourceArgs{
			ApiVersion: pulumi.String("kustomize.toolkit.fluxcd.io/v1"),
			Kind:       pulumi.String("Kustomization"),
			Metadata: &metav1.ObjectMetaArgs{
				Name: pulumi.String("bootstrap-kustomization"),
				Labels: pulumi.StringMap{
					"backstage.io/kubernetes-id": pulumi.String("gitops-cluster"),
				},
				Namespace: boostrapRepo.Metadata.Namespace(),
			},
			OtherFields: kubernetes.UntypedArgs{
				"spec": pulumi.Map{
					"force":    pulumi.Bool(false),
					"interval": pulumi.String("1m"),
					"prune":    pulumi.Bool(true),
					"path":     pulumi.String("./flux/clusters/aws-gitops-platform"),
					"sourceRef": pulumi.Map{
						"kind":      boostrapRepo.Kind,
						"name":      boostrapRepo.Metadata.Name(),
						"namespace": boostrapRepo.Metadata.Namespace(),
					},
					"targetNamespace": boostrapRepo.Metadata.Namespace(),
				},
			},
		}, pulumi.Provider(k8sProvider))
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
