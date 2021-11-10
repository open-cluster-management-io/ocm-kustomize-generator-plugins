// Copyright Contributors to the Open Cluster Management project
package internal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/open-cluster-management/policy-generator-plugin/internal/types"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	configPolicyKind           = "ConfigurationPolicy"
	policyAPIVersion           = "policy.open-cluster-management.io/v1"
	policyKind                 = "Policy"
	placementBindingAPIVersion = "policy.open-cluster-management.io/v1"
	placementBindingKind       = "PlacementBinding"
	placementRuleAPIVersion    = "apps.open-cluster-management.io/v1"
	placementRuleKind          = "PlacementRule"
	placementAPIVersion        = "cluster.open-cluster-management.io/v1alpha1"
	placementKind              = "Placement"
	maxObjectNameLength        = 63
)

// Plugin is used to store the PolicyGenerator configuration and the methods to generate the
// desired policies.
type Plugin struct {
	Metadata struct {
		Name string `json:"name,omitempty" yaml:"name,omitempty"`
	} `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	PlacementBindingDefaults struct {
		Name string `json:"name,omitempty" yaml:"name,omitempty"`
	} `json:"placementBindingDefaults,omitempty" yaml:"placementBindingDefaults,omitempty"`
	PolicyDefaults types.PolicyDefaults `json:"policyDefaults,omitempty" yaml:"policyDefaults,omitempty"`
	Policies       []types.PolicyConfig `json:"policies" yaml:"policies"`
	// A set of all placement rule names that have been processed or generated
	allPlrs map[string]bool
	// This is a mapping of cluster selectors formatted as the return value of getCsKey to placement
	// rule names. This is used to find common cluster selectors that can be consolidated to a
	// single placement rule.
	csToPlr      map[string]string
	outputBuffer bytes.Buffer
	// Track placement kind (we only expect to have one kind)
	usingPlR bool
	// A set of processed placement rules from external placement rules (either
	// Placement.PlacementRulePath or Placement.PlacementPath)
	processedPlrs map[string]bool
}

var defaults = types.PolicyDefaults{
	Categories:        []string{"CM Configuration Management"},
	ComplianceType:    "musthave",
	Controls:          []string{"CM-2 Baseline Configuration"},
	RemediationAction: "inform",
	Severity:          "low",
	Standards:         []string{"NIST SP 800-53"},
}

// Config validates the input PolicyGenerator configuration, applies any missing defaults, and
// configures the Policy object.
func (p *Plugin) Config(config []byte) error {
	err := yaml.Unmarshal(config, p)
	const errTemplate = "the PolicyGenerator configuration file is invalid: %w"
	if err != nil {
		return fmt.Errorf(errTemplate, err)
	}

	var unmarshaledConfig map[string]interface{}
	err = yaml.Unmarshal(config, &unmarshaledConfig)
	if err != nil {
		return fmt.Errorf(errTemplate, err)
	}
	p.applyDefaults(unmarshaledConfig)

	return p.assertValidConfig()
}

// Generate generates the policies, placement rules, and placement bindings and returns them as
// a single YAML file as a byte array. An error is returned if they cannot be created.
func (p *Plugin) Generate() ([]byte, error) {
	// Set the default empty values to the fields that track state
	p.allPlrs = map[string]bool{}
	p.csToPlr = map[string]string{}
	p.outputBuffer = bytes.Buffer{}
	p.processedPlrs = map[string]bool{}

	for i := range p.Policies {
		err := p.createPolicy(&p.Policies[i])
		if err != nil {
			return nil, err
		}
	}

	// Keep track of which placement rule maps to which policy. This will be used to determine
	// how many placement bindings are required since one per placement rule is required.
	plrNameToPolicyIdxs := map[string][]int{}
	for i := range p.Policies {
		plrName, err := p.createPlacementRule(&p.Policies[i])
		if err != nil {
			return nil, err
		}
		plrNameToPolicyIdxs[plrName] = append(plrNameToPolicyIdxs[plrName], i)
	}

	// Sort the keys of plrNameToPolicyIdxs so that the policy bindings are generated in a
	// consistent order.
	plrNames := make([]string, len(plrNameToPolicyIdxs))
	i := 0
	for k := range plrNameToPolicyIdxs {
		plrNames[i] = k
		i++
	}
	sort.Strings(plrNames)

	plcBindingCount := 0
	for _, plrName := range plrNames {
		// Determine which policies to be included in the placement binding.
		policyConfs := []*types.PolicyConfig{}
		for _, i := range plrNameToPolicyIdxs[plrName] {
			policyConfs = append(policyConfs, &p.Policies[i])
		}

		// If there is more than one policy associated with a placement rule but no default binding name
		// specified, throw an error
		if len(policyConfs) > 1 && p.PlacementBindingDefaults.Name == "" {
			return nil, fmt.Errorf(
				"placementBindingDefaults.name must be set but is empty (multiple policies were found for the PlacementBinding to placement '%s')",
				plrName,
			)
		}

		var bindingName string
		// If there is only one policy, use the policy name if there is no default
		// binding name specified
		if len(policyConfs) == 1 && p.PlacementBindingDefaults.Name == "" {
			bindingName = "binding-" + policyConfs[0].Name
		} else {
			plcBindingCount++
			// If there are multiple policies, use the default placement binding name
			// but append a number to it so it's a unique name.
			if plcBindingCount == 1 {
				bindingName = p.PlacementBindingDefaults.Name
			} else {
				bindingName = fmt.Sprintf("%s%d", p.PlacementBindingDefaults.Name, plcBindingCount)
			}
		}

		err := p.createPlacementBinding(bindingName, plrName, policyConfs)
		if err != nil {
			return nil, fmt.Errorf("failed to create a placement binding: %w", err)
		}
	}

	return p.outputBuffer.Bytes(), nil
}

func getDefaultBool(config map[string]interface{}, key string) (value bool, set bool) {
	defaults, ok := config["policyDefaults"].(map[string]interface{})
	if ok {
		value, set = defaults[key].(bool)

		return
	}

	return false, false
}

func getPolicyBool(
	config map[string]interface{}, policyIndex int, key string,
) (value bool, set bool) {
	policies, ok := config["policies"].([]interface{})
	if !ok {
		return false, false
	}

	if len(policies)-1 < policyIndex {
		return false, false
	}

	policy, ok := policies[policyIndex].(map[string]interface{})
	if !ok {
		return false, false
	}

	value, set = policy[key].(bool)

	return
}

// applyDefaults applies any missing defaults under Policy.PlacementBindingDefaults and
// Policy.PolicyDefaults. It then applies the defaults and user provided defaults on each
// policy entry if they are not overridden by the user. The input unmarshaledConfig is used
// in situations where it is necessary to know if an explicit false is provided rather than
// rely on the default Go value on the Plugin struct.
func (p *Plugin) applyDefaults(unmarshaledConfig map[string]interface{}) {
	if len(p.Policies) == 0 {
		return
	}

	// Set defaults to the defaults that aren't overridden
	if p.PolicyDefaults.Categories == nil {
		p.PolicyDefaults.Categories = defaults.Categories
	}

	if p.PolicyDefaults.ComplianceType == "" {
		p.PolicyDefaults.ComplianceType = defaults.ComplianceType
	}

	if p.PolicyDefaults.Controls == nil {
		p.PolicyDefaults.Controls = defaults.Controls
	}

	// Policy expanders default to true unless explicitly set in the config.
	// Gatekeeper policy expander policyDefault
	igvValue, setIgv := getDefaultBool(unmarshaledConfig, "informGatekeeperPolicies")
	if setIgv {
		p.PolicyDefaults.InformGatekeeperPolicies = igvValue
	} else {
		p.PolicyDefaults.InformGatekeeperPolicies = true
	}
	// Kyverno policy expander policyDefault
	ikvValue, setIkv := getDefaultBool(unmarshaledConfig, "informKyvernoPolicies")
	if setIkv {
		p.PolicyDefaults.InformKyvernoPolicies = ikvValue
	} else {
		p.PolicyDefaults.InformKyvernoPolicies = true
	}

	consolidatedValue, setConsolidated := getDefaultBool(unmarshaledConfig, "consolidateManifests")
	if setConsolidated {
		p.PolicyDefaults.ConsolidateManifests = consolidatedValue
	} else {
		p.PolicyDefaults.ConsolidateManifests = true
	}

	if p.PolicyDefaults.RemediationAction == "" {
		p.PolicyDefaults.RemediationAction = defaults.RemediationAction
	}

	if p.PolicyDefaults.Severity == "" {
		p.PolicyDefaults.Severity = defaults.Severity
	}

	if p.PolicyDefaults.Standards == nil {
		p.PolicyDefaults.Standards = defaults.Standards
	}

	for i := range p.Policies {
		policy := &p.Policies[i]
		if policy.Categories == nil {
			policy.Categories = p.PolicyDefaults.Categories
		}

		if policy.ComplianceType == "" {
			policy.ComplianceType = p.PolicyDefaults.ComplianceType
		}

		if policy.Controls == nil {
			policy.Controls = p.PolicyDefaults.Controls
		}

		// Policy expanders default to the policy default unless explicitly set.
		// Gatekeeper policy expander policy override
		igvValue, setIgv := getPolicyBool(unmarshaledConfig, i, "informGatekeeperPolicies")
		if setIgv {
			policy.InformGatekeeperPolicies = igvValue
		} else {
			policy.InformGatekeeperPolicies = p.PolicyDefaults.InformGatekeeperPolicies
		}
		// Kyverno policy expander policy override
		ikvValue, setIkv := getPolicyBool(unmarshaledConfig, i, "informKyvernoPolicies")
		if setIkv {
			policy.InformKyvernoPolicies = ikvValue
		} else {
			policy.InformKyvernoPolicies = p.PolicyDefaults.InformKyvernoPolicies
		}

		consolidatedValue, setConsolidated := getPolicyBool(unmarshaledConfig, i, "consolidateManifests")
		if setConsolidated {
			policy.ConsolidateManifests = consolidatedValue
		} else {
			policy.ConsolidateManifests = p.PolicyDefaults.ConsolidateManifests
		}

		// Determine whether defaults are set for placement
		plcDefaultSet := len(p.PolicyDefaults.Placement.LabelSelector) != 0 || p.PolicyDefaults.Placement.PlacementPath != ""
		plrDefaultSet := len(p.PolicyDefaults.Placement.ClusterSelectors) != 0 || p.PolicyDefaults.Placement.PlacementRulePath != ""

		// If both cluster label selectors and placement path aren't set, then use the defaults with a
		// priority on placement path.
		if len(policy.Placement.LabelSelector) == 0 && policy.Placement.PlacementPath == "" && plcDefaultSet {
			if p.PolicyDefaults.Placement.PlacementPath != "" {
				policy.Placement.PlacementPath = p.PolicyDefaults.Placement.PlacementPath
			} else if len(p.PolicyDefaults.Placement.LabelSelector) > 0 {
				policy.Placement.LabelSelector = p.PolicyDefaults.Placement.LabelSelector
			}
			// Else if both cluster selectors and placement rule path aren't set, then use the defaults with a
			// priority on placement rule path.
		} else if len(policy.Placement.ClusterSelectors) == 0 && policy.Placement.PlacementRulePath == "" && plrDefaultSet {
			if p.PolicyDefaults.Placement.PlacementRulePath != "" {
				policy.Placement.PlacementRulePath = p.PolicyDefaults.Placement.PlacementRulePath
			} else if len(p.PolicyDefaults.Placement.ClusterSelectors) > 0 {
				policy.Placement.ClusterSelectors = p.PolicyDefaults.Placement.ClusterSelectors
			}
		}

		// Only use defaults when when both include and exclude are not set on the policy
		nsSelector := policy.NamespaceSelector
		defNsSelector := p.PolicyDefaults.NamespaceSelector
		if nsSelector.Exclude == nil && nsSelector.Include == nil {
			policy.NamespaceSelector = defNsSelector
		}

		if policy.RemediationAction == "" {
			policy.RemediationAction = p.PolicyDefaults.RemediationAction
		}

		if policy.Severity == "" {
			policy.Severity = p.PolicyDefaults.Severity
		}

		if policy.Standards == nil {
			policy.Standards = p.PolicyDefaults.Standards
		}

		for i := range policy.Manifests {
			if policy.Manifests[i].ComplianceType == "" {
				policy.Manifests[i].ComplianceType = policy.ComplianceType
			}
		}
	}
}

// assertValidConfig verifies that the user provided configuration has all the
// required fields. Note that this should be run only after applyDefaults is run.
func (p *Plugin) assertValidConfig() error {
	if p.PolicyDefaults.Namespace == "" {
		return errors.New("policyDefaults.namespace is empty but it must be set")
	}

	// Validate default Placement settings
	if p.PolicyDefaults.Placement.PlacementRulePath != "" && p.PolicyDefaults.Placement.PlacementPath != "" {
		return errors.New(
			"policyDefaults must provide only one of placement.placementPath or placement.placementRulePath",
		)
	}
	if len(p.PolicyDefaults.Placement.ClusterSelectors) > 0 && len(p.PolicyDefaults.Placement.LabelSelector) > 0 {
		return errors.New(
			"policyDefaults must provide only one of placement.labelSelector or placement.clusterSelectors",
		)
	}
	if (len(p.PolicyDefaults.Placement.LabelSelector) != 0 || len(p.PolicyDefaults.Placement.ClusterSelectors) != 0) &&
		(p.PolicyDefaults.Placement.PlacementRulePath != "" || p.PolicyDefaults.Placement.PlacementPath != "") {
		return errors.New(
			"policyDefaults may not specify a placement selector and placement path together",
		)
	}

	if len(p.Policies) == 0 {
		return errors.New("policies is empty but it must be set")
	}

	seen := map[string]bool{}
	plCount := struct {
		plc int
		plr int
	}{}
	for i := range p.Policies {
		policy := &p.Policies[i]
		if policy.Name == "" {
			return fmt.Errorf(
				"each policy must have a name set, but did not find a name at policy array index %d", i,
			)
		}

		if seen[policy.Name] {
			return fmt.Errorf(
				"each policy must have a unique name set, but found a duplicate name: %s", policy.Name,
			)
		}
		seen[policy.Name] = true

		if len(p.PolicyDefaults.Namespace+"."+policy.Name) > maxObjectNameLength {
			return fmt.Errorf("the policy namespace and name cannot be more than 63 characters: %s.%s",
				p.PolicyDefaults.Namespace, policy.Name)
		}

		if len(policy.Manifests) == 0 {
			return fmt.Errorf(
				"each policy must have at least one manifest, but found none in policy %s", policy.Name,
			)
		}

		for _, manifest := range policy.Manifests {
			if manifest.Path == "" {
				return fmt.Errorf(
					"each policy manifest entry must have path set, but did not find a path in policy %s",
					policy.Name,
				)
			}

			_, err := os.Stat(manifest.Path)
			if err != nil {
				return fmt.Errorf(
					"could not read the manifest path %s in policy %s", manifest.Path, policy.Name,
				)
			}
		}

		// Validate policy Placement settings
		if policy.Placement.PlacementRulePath != "" && policy.Placement.PlacementPath != "" {
			return fmt.Errorf(
				"policy %s must provide only one of placementRulePath or placementPath", policy.Name,
			)
		}

		if len(policy.Placement.ClusterSelectors) > 0 && len(policy.Placement.LabelSelector) > 0 {
			return fmt.Errorf(
				"policy %s must provide only one of placement.labelSelector or placement.clusterselectors",
				policy.Name,
			)
		}
		if (len(policy.Placement.ClusterSelectors) != 0 || len(policy.Placement.LabelSelector) != 0) &&
			(policy.Placement.PlacementRulePath != "" || policy.Placement.PlacementPath != "") {
			return fmt.Errorf(
				"policy %s may not specify a placement selector and placement path together", policy.Name,
			)
		}
		if policy.Placement.PlacementRulePath != "" {
			_, err := os.Stat(policy.Placement.PlacementRulePath)
			if err != nil {
				return fmt.Errorf(
					"could not read the placement rule path %s",
					policy.Placement.PlacementRulePath,
				)
			}
		}
		if policy.Placement.PlacementPath != "" {
			_, err := os.Stat(policy.Placement.PlacementPath)
			if err != nil {
				return fmt.Errorf(
					"could not read the placement rule path %s",
					policy.Placement.PlacementPath,
				)
			}
		}

		foundPl := false
		if len(policy.Placement.LabelSelector) != 0 || policy.Placement.PlacementPath != "" {
			plCount.plc++
			foundPl = true
		}
		if len(policy.Placement.ClusterSelectors) != 0 || policy.Placement.PlacementRulePath != "" {
			plCount.plr++
			if foundPl {
				return fmt.Errorf(
					"policy '%s' may not use both Placement and PlacementRule kinds", policy.Name,
				)
			}
		}
	}

	// Validate only one type of placement kind is in use
	if plCount.plc != 0 && plCount.plr != 0 {
		return fmt.Errorf(
			"may not use a mix of Placement and PlacementRule for policies; found %d Placement and %d PlacementRule",
			plCount.plc, plCount.plr,
		)
	}

	p.usingPlR = plCount.plr != 0

	return nil
}

// createPolicy will generate the root policy based on the PolicyGenerator configuration.
// The generated policy is written to the plugin's output buffer. An error is returned if the
// manifests specified in the configuration are invalid or can't be read.
func (p *Plugin) createPolicy(policyConf *types.PolicyConfig) error {
	policyTemplates, err := getPolicyTemplates(policyConf)
	if err != nil {
		return err
	}

	policy := map[string]interface{}{
		"apiVersion": policyAPIVersion,
		"kind":       policyKind,
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				"policy.open-cluster-management.io/categories": strings.Join(policyConf.Categories, ","),
				"policy.open-cluster-management.io/controls":   strings.Join(policyConf.Controls, ","),
				"policy.open-cluster-management.io/standards":  strings.Join(policyConf.Standards, ","),
			},
			"name":      policyConf.Name,
			"namespace": p.PolicyDefaults.Namespace,
		},
		"spec": map[string]interface{}{
			"disabled":         policyConf.Disabled,
			"policy-templates": policyTemplates,
		},
	}

	policyYAML, err := yaml.Marshal(policy)
	if err != nil {
		return fmt.Errorf(
			"an unexpected error occurred when converting the policy to YAML: %w", err,
		)
	}

	p.outputBuffer.Write([]byte("---\n"))
	p.outputBuffer.Write(policyYAML)

	return nil
}

// getPlcFromPath finds the placement manifest in the input manifest file. It will return the name
// of the placement, the unmarshaled placement manifest, and an error. An error is returned if the
// placement manifest cannot be found or is invalid.
func (p *Plugin) getPlcFromPath(plcPath string) (string, map[string]interface{}, error) {
	manifests, err := unmarshalManifestFile(plcPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read the placement rule: %w", err)
	}

	var name string
	var rule map[string]interface{}
	for _, manifest := range *manifests {
		kind, _, _ := unstructured.NestedString(manifest, "kind")
		if kind != placementRuleKind && kind != placementKind {
			continue
		}

		// Validate PlacementRule Kind given in manifest
		if kind == placementRuleKind {
			if !p.usingPlR {
				return "", nil, fmt.Errorf(
					"the placement %s specified a placementRule kind but expected a placement kind", plcPath,
				)
			}
		}

		// Validate Placement Kind given in manifest
		if kind == placementKind {
			if p.usingPlR {
				return "", nil, fmt.Errorf(
					"the placement %s specified a placement kind but expected a placementRule kind", plcPath,
				)
			}
		}

		var found bool
		name, found, err = unstructured.NestedString(manifest, "metadata", "name")
		if !found || err != nil {
			return "", nil, fmt.Errorf("the placement %s must have a name set", plrPath)
		}

		var namespace string
		namespace, found, err = unstructured.NestedString(manifest, "metadata", "namespace")
		if !found || err != nil {
			return "", nil, fmt.Errorf("the placement %s must have a namespace set", plrPath)
		}

		if namespace != p.PolicyDefaults.Namespace {
			err = fmt.Errorf(
				"the placement %s must have the same namespace as the policy (%s)",
				plrPath,
				p.PolicyDefaults.Namespace,
			)

			return "", nil, err
		}

		rule = manifest

		break
	}

	if name == "" {
		err = fmt.Errorf(
			"the placement manifest %s did not have a placement rule", plrPath,
		)

		return "", nil, err
	}

	return name, rule, nil
}

// getCsKey generates the key for the policy's cluster selectors to be used in Policies.csToPlr.
func getCsKey(policyConf *types.PolicyConfig) string {
	return fmt.Sprintf("%#v", policyConf.Placement.ClusterSelectors)
}

// getPlrName will generate a placement rule name for the policy. If the placement rule has
// previously been generated, skip will be true.
func (p *Plugin) getPlrName(policyConf *types.PolicyConfig) (name string, skip bool) {
	if policyConf.Placement.Name != "" {
		// If the policy explicitly specifies a placement rule name, use it
		return policyConf.Placement.Name, false
	} else if p.PolicyDefaults.Placement.Name != "" {
		// If the policy doesn't explicitly specify a placement rule name, and there is a
		// default placement rule name set, check if one has already been generated for these
		// cluster selectors
		csKey := getCsKey(policyConf)
		if _, ok := p.csToPlr[csKey]; ok {
			// Just reuse the previously created placement rule with the same cluster selectors
			return p.csToPlr[csKey], true
		}
		// If the policy doesn't explicitly specify a placement rule name, and there is a
		// default placement rule name, use that
		if len(p.csToPlr) == 0 {
			// If this is the first generated placement rule, just use it as is
			return p.PolicyDefaults.Placement.Name, false
		}
		// If there is already one or more generated placement rules, increment the name
		return fmt.Sprintf("%s%d", p.PolicyDefaults.Placement.Name, len(p.csToPlr)+1), false
	}
	// Default to a placement rule per policy
	return "placement-" + policyConf.Name, false
}

// createPlacementRule creates a placement rule for the input policy configuration by writing it to
// the policy generator's output buffer. The name of the placement rule or an error is returned.
// If the placement rule has already been generated, it will be reused and not added to the
// policy generator's output buffer. An error is returned if the placement rule cannot be created.
func (p *Plugin) createPlacementRule(policyConf *types.PolicyConfig) (
	name string, err error,
) {
	plrPath := policyConf.Placement.PlacementRulePath
	plcPath := policyConf.Placement.PlacementPath
	var placement map[string]interface{}
	// If a path to a placement is provided, find the placement and reuse it.
	if plrPath != "" || plPath != "" {
		var resolvedPlPath string
		if plrPath != "" {
			resolvedPlPath = plrPath
		} else {
			resolvedPlPath = plPath
		}
		name, placement, err = p.getPlcFromPath(resolvedPlPath)
		if err != nil {
			return
		}

		// processedPlrs keeps track of which placement rules have been seen by name. This is so
		// that if the same placementRulePath is provided for multiple policies, it's not re-included
		// in the generated output of the plugin.
		if p.processedPlrs[name] {
			return
		}

		p.processedPlrs[name] = true
	} else {
		var skip bool
		name, skip = p.getPlrName(policyConf)
		if skip {
			return
		}

		// Determine which selectors to use
		var resolvedSelectors map[string]string
		if len(policyConf.Placement.ClusterSelectors) > 0 {
			resolvedSelectors = policyConf.Placement.ClusterSelectors
		} else if len(policyConf.Placement.LabelSelector) > 0 {
			resolvedSelectors = policyConf.Placement.LabelSelector
		}

		// Sort the keys so that the match expressions can be ordered based on the label name
		keys := make([]string, 0, len(resolvedSelectors))
		for key := range resolvedSelectors {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		matchExpressions := []map[string]interface{}{}
		for _, label := range keys {
			matchExpression := map[string]interface{}{
				"key": label,
			}
			if resolvedSelectors[label] == "" {
				matchExpression["operator"] = "Exist"
			} else {
				matchExpression["operator"] = "In"
				matchExpression["values"] = []string{resolvedSelectors[label]}
			}
			matchExpressions = append(matchExpressions, matchExpression)
		}

		if p.usingPlR {
			rule = map[string]interface{}{
				"apiVersion": placementRuleAPIVersion,
				"kind":       placementRuleKind,
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": p.PolicyDefaults.Namespace,
				},
				"spec": map[string]interface{}{
					"clusterConditions": []map[string]string{
						{"status": "True", "type": "ManagedClusterConditionAvailable"},
					},
					"clusterSelector": map[string]interface{}{
						"matchExpressions": matchExpressions,
					},
				},
			}
		} else {
			rule = map[string]interface{}{
				"apiVersion": placementAPIVersion,
				"kind":       placementKind,
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": p.PolicyDefaults.Namespace,
				},
				"spec": map[string]interface{}{
					"predicates": []map[string]interface{}{
						{
							"requiredClusterSelector": map[string]interface{}{
								"labelSelector": map[string]interface{}{
									"matchExpressions": matchExpressions,
								},
							},
						},
					},
				},
			}
		}

		csKey := getCsKey(policyConf)
		p.csToPlr[csKey] = name
	}

	if p.allPlrs[name] {
		return "", fmt.Errorf("a duplicate placement rule name was detected: %s", name)
	}
	p.allPlrs[name] = true

	var ruleYAML []byte
	ruleYAML, err = yaml.Marshal(rule)
	if err != nil {
		err = fmt.Errorf(
			"an unexpected error occurred when converting the placement rule to YAML: %w", err,
		)

		return
	}

	p.outputBuffer.Write([]byte("---\n"))
	p.outputBuffer.Write(ruleYAML)

	return
}

// createPlacementBinding creates a placement binding for the input placement rule and policies by
// writing it to the policy generator's output buffer. An error is returned if the placement binding
// cannot be created.
func (p *Plugin) createPlacementBinding(
	bindingName, plrName string, policyConfs []*types.PolicyConfig,
) error {
	subjects := make([]map[string]string, 0, len(policyConfs))
	for _, policyConf := range policyConfs {
		subject := map[string]string{
			// Remove the version at the end
			"apiGroup": strings.Split(policyAPIVersion, "/")[0],
			"kind":     policyKind,
			"name":     policyConf.Name,
		}
		subjects = append(subjects, subject)
	}

	if p.usingPlR {
	} else {
		resolvedPlRKind = placementKind
		resolvedPlRAPIVersion = placementAPIVersion
	}

	binding := map[string]interface{}{
		"apiVersion": placementBindingAPIVersion,
		"kind":       placementBindingKind,
		"metadata": map[string]interface{}{
			"name":      bindingName,
			"namespace": p.PolicyDefaults.Namespace,
		},
		"placementRef": map[string]string{
			// Remove the version at the end
			"apiGroup": strings.Split(resolvedPlRAPIVersion, "/")[0],
			"name":     plrName,
			"kind":     resolvedPlRKind,
		},
		"subjects": subjects,
	}

	bindingYAML, err := yaml.Marshal(binding)
	if err != nil {
		return fmt.Errorf(
			"an unexpected error occurred when converting the placement binding to YAML: %w", err,
		)
	}

	p.outputBuffer.Write([]byte("---\n"))
	p.outputBuffer.Write(bindingYAML)

	return nil
}
