package builder

import (
	"fmt"

	cfn "github.com/aws/aws-sdk-go/service/cloudformation"
	gfniam "github.com/weaveworks/goformation/v4/cloudformation/iam"
	gfnt "github.com/weaveworks/goformation/v4/cloudformation/types"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/outputs"
	cft "github.com/weaveworks/eksctl/pkg/cfn/template"
	"github.com/weaveworks/eksctl/pkg/iam"
	iamoidc "github.com/weaveworks/eksctl/pkg/iam/oidc"
)

const (
	iamPolicyAmazonEKSClusterPolicy         = "AmazonEKSClusterPolicy"
	iamPolicyAmazonEKSVPCResourceController = "AmazonEKSVPCResourceController"

	iamPolicyAmazonEKSWorkerNodePolicy           = "AmazonEKSWorkerNodePolicy"
	iamPolicyAmazonEKSCNIPolicy                  = "AmazonEKS_CNI_Policy"
	iamPolicyAmazonEC2ContainerRegistryPowerUser = "AmazonEC2ContainerRegistryPowerUser"
	iamPolicyAmazonEC2ContainerRegistryReadOnly  = "AmazonEC2ContainerRegistryReadOnly"
	iamPolicyCloudWatchAgentServerPolicy         = "CloudWatchAgentServerPolicy"

	iamPolicyAmazonEKSFargatePodExecutionRolePolicy = "AmazonEKSFargatePodExecutionRolePolicy"
)

const (
	cfnIAMInstanceRoleName    = "NodeInstanceRole"
	cfnIAMInstanceProfileName = "NodeInstanceProfile"
)

var (
	iamDefaultNodePolicies = []string{
		iamPolicyAmazonEKSWorkerNodePolicy,
	}
)

func (c *resourceSet) attachAllowPolicy(name string, refRole *gfnt.Value, resources interface{}, actions []string) {
	c.newResource(name, &gfniam.Policy{
		PolicyName: makeName(name),
		Roles:      gfnt.NewSlice(refRole),
		PolicyDocument: cft.MakePolicyDocument(map[string]interface{}{
			"Effect":   "Allow",
			"Resource": resources,
			"Action":   actions,
		}),
	})
}

// WithIAM states, if IAM roles will be created or not
func (c *ClusterResourceSet) WithIAM() bool {
	return c.rs.withIAM
}

// WithNamedIAM states, if specifically named IAM roles will be created or not
func (c *ClusterResourceSet) WithNamedIAM() bool {
	return c.rs.withNamedIAM
}

func (c *ClusterResourceSet) addResourcesForIAM() {
	c.rs.withNamedIAM = false

	if api.IsSetAndNonEmptyString(c.spec.IAM.ServiceRoleARN) {
		c.rs.withIAM = false
		c.rs.defineOutputWithoutCollector(outputs.ClusterServiceRoleARN, c.spec.IAM.ServiceRoleARN, true)
		return
	}

	c.rs.withIAM = true

	managedPolicyArns := []string{
		iamPolicyAmazonEKSClusterPolicy,
	}
	if !api.IsDisabled(c.spec.IAM.VPCResourceControllerPolicy) {
		managedPolicyArns = append(managedPolicyArns, iamPolicyAmazonEKSVPCResourceController)
	}

	role := &gfniam.Role{
		AssumeRolePolicyDocument: cft.MakeAssumeRolePolicyDocumentForServices(
			MakeServiceRef("EKS"),
			// Ensure that EKS can schedule pods onto Fargate, should the user
			// define so-called "Fargate profiles" in order to do so:
			MakeServiceRef("EKSFargatePods"),
		),
		ManagedPolicyArns: gfnt.NewSlice(makePolicyARNs(managedPolicyArns...)...),
	}
	if api.IsSetAndNonEmptyString(c.spec.IAM.ServiceRolePermissionsBoundary) {
		role.PermissionsBoundary = gfnt.NewString(*c.spec.IAM.ServiceRolePermissionsBoundary)
	}
	refSR := c.newResource("ServiceRole", role)
	c.rs.attachAllowPolicy("PolicyCloudWatchMetrics", refSR, "*", []string{
		"cloudwatch:PutMetricData",
	})
	// These are potentially required for creating load balancers but aren't included in the
	// AmazonEKSClusterPolicy
	// See https://docs.aws.amazon.com/elasticloadbalancing/latest/userguide/elb-api-permissions.html#required-permissions-v2
	// and weaveworks/eksctl#2488
	c.rs.attachAllowPolicy("PolicyELBPermissions", refSR, "*", []string{
		"ec2:DescribeAccountAttributes",
		"ec2:DescribeAddresses",
		"ec2:DescribeInternetGateways",
	})

	c.rs.defineOutputFromAtt(outputs.ClusterServiceRoleARN, "ServiceRole", "Arn", true, func(v string) error {
		c.spec.IAM.ServiceRoleARN = &v
		return nil
	})
}

// WithIAM states, if IAM roles will be created or not
func (n *NodeGroupResourceSet) WithIAM() bool {
	return n.rs.withIAM
}

// WithNamedIAM states, if specifically named IAM roles will be created or not
func (n *NodeGroupResourceSet) WithNamedIAM() bool {
	return n.rs.withNamedIAM
}

func (n *NodeGroupResourceSet) addResourcesForIAM() error {
	if n.spec.IAM.InstanceProfileARN != "" {
		n.rs.withIAM = false
		n.rs.withNamedIAM = false

		// if instance profile is given, as well as the role, we simply use both and export the role
		// (which is needed in order to authorise the nodegroup)
		n.instanceProfileARN = gfnt.NewString(n.spec.IAM.InstanceProfileARN)
		if n.spec.IAM.InstanceRoleARN != "" {
			n.rs.defineOutputWithoutCollector(outputs.NodeGroupInstanceProfileARN, n.spec.IAM.InstanceProfileARN, true)
			n.rs.defineOutputWithoutCollector(outputs.NodeGroupInstanceRoleARN, n.spec.IAM.InstanceRoleARN, true)
			return nil
		}
		// if instance role is not given, export profile and use the getter to call importer function
		n.rs.defineOutput(outputs.NodeGroupInstanceProfileARN, n.spec.IAM.InstanceProfileARN, true, func(v string) error {
			return iam.ImportInstanceRoleFromProfileARN(n.provider, n.spec, v)
		})

		return nil
	}

	n.rs.withIAM = true

	if n.spec.IAM.InstanceRoleARN != "" {
		// if role is set, but profile isn't - create profile
		n.newResource(cfnIAMInstanceProfileName, &gfniam.InstanceProfile{
			Path:  gfnt.NewString("/"),
			Roles: gfnt.NewStringSlice(n.spec.IAM.InstanceRoleARN),
		})
		n.instanceProfileARN = gfnt.MakeFnGetAttString(cfnIAMInstanceProfileName, "Arn")
		n.rs.defineOutputFromAtt(outputs.NodeGroupInstanceProfileARN, cfnIAMInstanceProfileName, "Arn", true, func(v string) error {
			n.spec.IAM.InstanceProfileARN = v
			return nil
		})
		n.rs.defineOutputWithoutCollector(outputs.NodeGroupInstanceRoleARN, n.spec.IAM.InstanceRoleARN, true)
		return nil
	}

	// if neither role nor profile is given - create both

	if n.spec.IAM.InstanceRoleName != "" {
		// setting role name requires additional capabilities
		n.rs.withNamedIAM = true
	}

	if err := createRole(n.rs, n.clusterSpec.IAM, n.spec.IAM, false); err != nil {
		return err
	}

	n.newResource(cfnIAMInstanceProfileName, &gfniam.InstanceProfile{
		Path:  gfnt.NewString("/"),
		Roles: gfnt.NewSlice(gfnt.MakeRef(cfnIAMInstanceRoleName)),
	})
	n.instanceProfileARN = gfnt.MakeFnGetAttString(cfnIAMInstanceProfileName, "Arn")

	n.rs.defineOutputFromAtt(outputs.NodeGroupInstanceProfileARN, cfnIAMInstanceProfileName, "Arn", true, func(v string) error {
		n.spec.IAM.InstanceProfileARN = v
		return nil
	})
	n.rs.defineOutputFromAtt(outputs.NodeGroupInstanceRoleARN, cfnIAMInstanceRoleName, "Arn", true, func(v string) error {
		n.spec.IAM.InstanceRoleARN = v
		return nil
	})
	return nil
}

// IAMServiceAccountResourceSet holds iamserviceaccount stack build-time information
type IAMServiceAccountResourceSet struct {
	template *cft.Template
	spec     *api.ClusterIAMServiceAccount
	oidc     *iamoidc.OpenIDConnectManager
	outputs  *outputs.CollectorSet
}

// NewIAMServiceAccountResourceSet builds iamserviceaccount stack from the give spec
func NewIAMServiceAccountResourceSet(spec *api.ClusterIAMServiceAccount, oidc *iamoidc.OpenIDConnectManager) *IAMServiceAccountResourceSet {
	return &IAMServiceAccountResourceSet{
		template: cft.NewTemplate(),
		spec:     spec,
		oidc:     oidc,
	}
}

// WithIAM returns true
func (*IAMServiceAccountResourceSet) WithIAM() bool { return true }

// WithNamedIAM returns false
func (*IAMServiceAccountResourceSet) WithNamedIAM() bool { return false }

// AddAllResources adds all resources for the stack
func (rs *IAMServiceAccountResourceSet) AddAllResources() error {
	rs.template.Description = fmt.Sprintf(
		"IAM role for serviceaccount %q %s",
		rs.spec.NameString(),
		templateDescriptionSuffix,
	)

	// we use a single stack for each service account, but there maybe a few roles in it
	// so will need to give them unique names
	// we will need to consider using a large stack for all the roles, but that needs some
	// testing and potentially a better stack mutation strategy
	role := &cft.IAMRole{
		AssumeRolePolicyDocument: rs.oidc.MakeAssumeRolePolicyDocument(rs.spec.Namespace, rs.spec.Name),
		PermissionsBoundary:      rs.spec.PermissionsBoundary,
	}
	role.ManagedPolicyArns = append(role.ManagedPolicyArns, rs.spec.AttachPolicyARNs...)

	roleRef := rs.template.NewResource("Role1", role)

	// TODO: declare output collector automatically when all stack builders migrated to our template package
	rs.template.Outputs["Role1"] = cft.Output{
		Value: cft.MakeFnGetAttString("Role1.Arn"),
	}
	rs.outputs = outputs.NewCollectorSet(map[string]outputs.Collector{
		"Role1": func(v string) error {
			rs.spec.Status = &api.ClusterIAMServiceAccountStatus{
				RoleARN: &v,
			}
			return nil
		},
	})

	if len(rs.spec.AttachPolicy) != 0 {
		rs.template.AttachPolicy("Policy1", roleRef, rs.spec.AttachPolicy)
	}

	return nil
}

// RenderJSON will render iamserviceaccount stack as JSON
func (rs *IAMServiceAccountResourceSet) RenderJSON() ([]byte, error) {
	return rs.template.RenderJSON()
}

// GetAllOutputs will get all outputs from iamserviceaccount stack
func (rs *IAMServiceAccountResourceSet) GetAllOutputs(stack cfn.Stack) error {
	return rs.outputs.MustCollect(stack)
}
