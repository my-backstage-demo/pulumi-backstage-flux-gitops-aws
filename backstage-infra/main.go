package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/alb"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ecr"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi-docker/sdk/v4/go/docker"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

var (
	publicSubnetCidrs = []string{
		"10.0.0.64/27",
		"10.0.0.128/27",
	}
	availabilityZones = []string{
		"eu-central-1a",
		"eu-central-1b",
	}
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {

		infraStackRef, err := pulumi.NewStackReference(ctx, config.Get(ctx, "infraStackRef"), nil)
		if err != nil {
			return err
		}
		vpcId := infraStackRef.GetStringOutput(pulumi.String("vpc-id"))

		group, err := ec2.NewSecurityGroup(ctx, "pulumi-backstage-aws-sg", &ec2.SecurityGroupArgs{
			VpcId: vpcId,
			Ingress: ec2.SecurityGroupIngressArray{
				&ec2.SecurityGroupIngressArgs{
					Protocol: pulumi.String("tcp"),
					FromPort: pulumi.Int(80),
					ToPort:   pulumi.Int(80),
					CidrBlocks: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
					},
				},
				&ec2.SecurityGroupIngressArgs{
					Protocol: pulumi.String("tcp"),
					FromPort: pulumi.Int(5432),
					ToPort:   pulumi.Int(5432),
					CidrBlocks: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
					},
				},
			},
			Egress: ec2.SecurityGroupEgressArray{
				&ec2.SecurityGroupEgressArgs{
					Protocol: pulumi.String("-1"),
					FromPort: pulumi.Int(0),
					ToPort:   pulumi.Int(0),
					CidrBlocks: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
					},
				},
			},
		})
		if err != nil {
			return err
		}

		var publicSubnetIDs pulumi.StringArray

		// Create a subnet for each availability zone
		for i, az := range availabilityZones {
			publicSubnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("pulumi-backstage-aws-fargate-subnet-%d", i), &ec2.SubnetArgs{
				VpcId:                       vpcId,
				CidrBlock:                   pulumi.String(publicSubnetCidrs[i]),
				MapPublicIpOnLaunch:         pulumi.Bool(false),
				AssignIpv6AddressOnCreation: pulumi.Bool(false),
				AvailabilityZone:            pulumi.String(az),
				Tags: pulumi.StringMap{
					"Name": pulumi.Sprintf("pulumi-backstage-aws-subnet-fargate-%s", az),
				},
			})
			if err != nil {
				return err
			}
			_, err = ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("pulumi-backstage-aws-rt-association-%s", az), &ec2.RouteTableAssociationArgs{
				RouteTableId: infraStackRef.GetOutput(pulumi.String("route-table-id")).AsStringOutput(),
				SubnetId:     publicSubnet.ID(),
			})
			if err != nil {
				return err
			}
			publicSubnetIDs = append(publicSubnetIDs, publicSubnet.ID())
		}

		loadBalancer, err := alb.NewLoadBalancer(ctx, "pulumi-backstage-aws-alb", &alb.LoadBalancerArgs{
			Subnets:          publicSubnetIDs,
			LoadBalancerType: pulumi.String("application"),
			SecurityGroups: pulumi.StringArray{
				group.ID(),
			},
			Name: pulumi.String("pulumi-backstage"),
		})
		if err != nil {
			return err
		}

		targetGroup, err := alb.NewTargetGroup(ctx, "pulumi-backstage-aws-alb-target-group", &alb.TargetGroupArgs{
			Port:       pulumi.Int(80),
			Protocol:   pulumi.String("HTTP"),
			TargetType: pulumi.String("ip"),
			Name:       pulumi.String("pulumi-backstage"),
			VpcId:      vpcId,
		})
		if err != nil {
			return err
		}
		allListener, err := alb.NewListener(ctx, "pulumi-backstage-aws-alb-listener", &alb.ListenerArgs{
			LoadBalancerArn: loadBalancer.Arn,
			Port:            pulumi.Int(80),
			Protocol:        pulumi.String("HTTP"),
			DefaultActions: alb.ListenerDefaultActionArray{
				&alb.ListenerDefaultActionArgs{
					Type:           pulumi.String("forward"),
					TargetGroupArn: targetGroup.Arn,
				},
			},
		})
		if err != nil {
			return err
		}

		subnetGroup, err := rds.NewSubnetGroup(ctx, "pulumi-backstage-aws-rds-subnet-group", &rds.SubnetGroupArgs{
			SubnetIds: publicSubnetIDs,
		})
		if err != nil {
			return err
		}

		instance, err := rds.NewInstance(ctx, "pulumi-backstage-aws-rds", &rds.InstanceArgs{
			AllocatedStorage:   pulumi.Int(5),
			InstanceClass:      rds.InstanceType_T3_Micro,
			Engine:             pulumi.String("postgres"),
			EngineVersion:      pulumi.String("15.4"),
			Username:           pulumi.String("backstage"),
			Password:           pulumi.String("backstage"),
			PubliclyAccessible: pulumi.Bool(false),
			DbSubnetGroupName:  subnetGroup.Name,
			MultiAz:            pulumi.Bool(true),
			SkipFinalSnapshot:  pulumi.Bool(true),
			VpcSecurityGroupIds: pulumi.StringArray{
				group.ID(),
			},
		})
		if err != nil {
			return err
		}
		ctx.Export("rds", instance.Endpoint)

		repository, err := ecr.NewRepository(ctx, "pulumi-backstage-repository", &ecr.RepositoryArgs{
			Name:        pulumi.String("backstage"),
			ForceDelete: pulumi.Bool(true),
		})
		if err != nil {
			return err
		}

		_, err = ecr.NewLifecyclePolicy(ctx, "pulumi-backstage-lifecycle-policy", &ecr.LifecyclePolicyArgs{
			Repository: repository.Name,
			Policy: pulumi.String(`{
				"rules": [
					{
					   "rulePriority": 1,
					   "description": "keep last 10 images",
					   "selection": {
						   "tagStatus": "any",
						   "countType": "imageCountMoreThan",
						   "countNumber": 10
					   },
					   "action": {
						   "type": "expire"
					   }
					}
				]
			}`),
		})
		if err != nil {
			return err
		}

		ecsAssumeRolePolicyResult, err := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
			Statements: []iam.GetPolicyDocumentStatement{
				{
					Effect: pulumi.StringRef("Allow"),
					Actions: []string{
						"sts:AssumeRole",
					},
					Principals: []iam.GetPolicyDocumentStatementPrincipal{
						{
							Type: "Service",
							Identifiers: []string{
								"ecs-tasks.amazonaws.com",
							},
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}

		ecsRole, err := iam.NewRole(ctx, "pulumi-backstage-ecs-role", &iam.RoleArgs{
			AssumeRolePolicy: pulumi.String(ecsAssumeRolePolicyResult.Json),
		})
		if err != nil {
			return err
		}
		ecsPolicyResult, _ := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
			Statements: []iam.GetPolicyDocumentStatement{
				{
					Effect: pulumi.StringRef("Allow"),
					Actions: []string{
						"rds-db:*",
						"s3:*",
						"ecr:*",
						"rds:*",
						"ecs:*",
						"ec2:*",
						"eks:*",
						"iam:*",
						"lambda:*",
						"apigateway:*",
						"ssm:*",
						"autoscaling-plans:*",
						"autoscaling:*",
						"cloudformation:*",
					},
					Resources: []string{
						"*",
					},
				},
			},
		})

		ecsIAMPolicy, err := iam.NewPolicy(ctx, "pulumi-backstage-ecs-policy", &iam.PolicyArgs{
			Policy: pulumi.String(ecsPolicyResult.Json),
		})
		if err != nil {
			return err
		}

		_, err = iam.NewRolePolicyAttachment(ctx, "pulumi-backstage-ecs-role-policy-attachment", &iam.RolePolicyAttachmentArgs{
			PolicyArn: ecsIAMPolicy.Arn,
			Role:      ecsRole.Name,
		})

		taskExecutionResult, err := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
			Statements: []iam.GetPolicyDocumentStatement{
				{
					Effect: pulumi.StringRef("Allow"),
					Actions: []string{
						"sts:AssumeRole",
					},
					Principals: []iam.GetPolicyDocumentStatementPrincipal{
						{
							Type: "Service",
							Identifiers: []string{
								"ecs-tasks.amazonaws.com",
							},
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}

		taskExecutionRole, err := iam.NewRole(ctx, "pulumi-backstage-ecs-task-execution-role", &iam.RoleArgs{
			AssumeRolePolicy: pulumi.String(taskExecutionResult.Json),
		})
		if err != nil {
			return err
		}

		_, err = iam.NewRolePolicyAttachment(ctx, "pulumi-backstage-ecs-task-execution-role-policy-attachment", &iam.RolePolicyAttachmentArgs{
			PolicyArn: pulumi.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
			Role:      taskExecutionRole.Name,
		})

		registryInfo := repository.RegistryId.ApplyT(func(id string) (docker.Registry, error) {
			creds, err := ecr.GetCredentials(ctx, &ecr.GetCredentialsArgs{RegistryId: id})
			if err != nil {
				return docker.Registry{}, err
			}
			decoded, err := base64.StdEncoding.DecodeString(creds.AuthorizationToken)
			if err != nil {
				return docker.Registry{}, err
			}
			parts := strings.Split(string(decoded), ":")
			if len(parts) != 2 {
				return docker.Registry{}, errors.New("Invalid credentials")
			}

			return docker.Registry{
				Server:   &creds.ProxyEndpoint,
				Username: &parts[0],
				Password: &parts[1],
			}, nil
		}).(docker.RegistryOutput)

		//docker build ../.. -f Dockerfile --tag backstage
		backstageImage, err := docker.NewImage(ctx, "pulumi-backstage-image", &docker.ImageArgs{
			Build: docker.DockerBuildArgs{
				Context:        pulumi.String("./backstage"),
				Platform:       pulumi.String("linux/amd64"),
				BuilderVersion: docker.BuilderVersionBuilderBuildKit,
				Dockerfile:     pulumi.String("./backstage/packages/backend/Dockerfile"),
			},
			ImageName: repository.RepositoryUrl,
			Registry:  registryInfo,
		})
		if err != nil {
			return err
		}
		ctx.Export("backstage-image", backstageImage.RepoDigest)

		cluster, err := ecs.NewCluster(ctx, "pulumi-backstage-ecs-cluster", &ecs.ClusterArgs{
			Name: pulumi.String("backstage"),
		})
		if err != nil {
			return err
		}

		logGroup, err := cloudwatch.NewLogGroup(ctx, "pulumi-backstage-log-group", &cloudwatch.LogGroupArgs{
			Name:            pulumi.String("backstage-log"),
			RetentionInDays: pulumi.Int(7),
		})
		if err != nil {
			return err
		}

		backstageECSTask, err := ecs.NewTaskDefinition(ctx, "pulumi-backstage-ecs-task", &ecs.TaskDefinitionArgs{
			Family: pulumi.String("backstage"),
			EphemeralStorage: &ecs.TaskDefinitionEphemeralStorageArgs{
				SizeInGib: pulumi.Int(100),
			},
			ContainerDefinitions: pulumi.Sprintf(`[
    {
      "name": "app-first-task",
      "image": "%s",
      "essential": true,
      "environment": [
			{
				"name": "POSTGRES_HOST",
				"value": "%s"	
			},
			{
				"name": "POSTGRES_PORT",
				"value": "%d"	
			},
			{
				"name": "POSTGRES_USER",	
				"value": "backstage"	
			},
			{
				"name": "POSTGRES_PASSWORD",
				"value": "backstage"	
			},
			{
				"name": "BACKSTAGE_BASE_URL",
				"value": "http://%s"
			},
			{
				"name": "WEBSITES_PORT",
				"value": "7007"	
			},
			{
				"name": "K8S_CLUSTER_URL",
				"value": "%s"
			},
			{
				"name": "K8S_CLUSTER_SA_TOKEN",
				"value": "%s"
			},
			{
				"name": "PULUMI_ACCESS_TOKEN",
				"value": "%s"
			}
	  ],
	  "logConfiguration": {
        "logDriver": "awslogs",
		"options": {
			"awslogs-group": "%s",
			"awslogs-region": "eu-central-1",
			"awslogs-stream-prefix": "backstage"
		}
	  },
      "portMappings": [
        {
          "containerPort": 7007,
          "hostPort": 7007
        }
      ]
    }
  ]
`, backstageImage.ImageName, instance.Address, instance.Port, loadBalancer.DnsName,
				infraStackRef.GetStringOutput(pulumi.String("gitops-platform-endpoint")), infraStackRef.GetStringOutput(pulumi.String("backstage-token")), config.GetSecret(ctx, "pulumi-pat"), logGroup.Name),
			RequiresCompatibilities: pulumi.StringArray{
				pulumi.String("FARGATE"),
			},
			NetworkMode:      pulumi.String("awsvpc"),
			Memory:           pulumi.String("6144"),
			Cpu:              pulumi.String("2048"),
			ExecutionRoleArn: taskExecutionRole.Arn,
			TaskRoleArn:      ecsRole.Arn,
		})
		if err != nil {
			return err
		}

		fargateSecurityGroup, err := ec2.NewSecurityGroup(ctx, "pulumi-backstage-aws-fargate-service-sg", &ec2.SecurityGroupArgs{
			VpcId: vpcId,
			Ingress: ec2.SecurityGroupIngressArray{
				&ec2.SecurityGroupIngressArgs{
					Protocol: pulumi.String("-1"),
					FromPort: pulumi.Int(0),
					ToPort:   pulumi.Int(0),
					CidrBlocks: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
					},
				},
			},
			Egress: ec2.SecurityGroupEgressArray{
				&ec2.SecurityGroupEgressArgs{
					Protocol: pulumi.String("-1"),
					FromPort: pulumi.Int(0),
					ToPort:   pulumi.Int(0),
					CidrBlocks: pulumi.StringArray{
						pulumi.String("0.0.0.0/0"),
					},
				},
			},
		})
		if err != nil {
			return err
		}

		_, err = ecs.NewService(ctx, "pulumi-backstage-service", &ecs.ServiceArgs{
			Cluster:        cluster.Arn,
			TaskDefinition: backstageECSTask.Arn,
			NetworkConfiguration: &ecs.ServiceNetworkConfigurationArgs{
				Subnets:        publicSubnetIDs,
				AssignPublicIp: pulumi.Bool(true),
				SecurityGroups: pulumi.StringArray{
					fargateSecurityGroup.ID(),
				},
			},
			LoadBalancers: ecs.ServiceLoadBalancerArray{
				&ecs.ServiceLoadBalancerArgs{
					TargetGroupArn: targetGroup.Arn,
					ContainerName:  pulumi.String("app-first-task"),
					ContainerPort:  pulumi.Int(7007),
				},
			},
			LaunchType:         pulumi.String("FARGATE"),
			DesiredCount:       pulumi.Int(1),
			SchedulingStrategy: pulumi.String("REPLICA"),
		}, pulumi.DependsOn([]pulumi.Resource{allListener}))
		if err != nil {
			return err
		}

		ctx.Export("url", loadBalancer.DnsName)
		return nil
	})

}
