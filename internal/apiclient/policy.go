package apiclient

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"

	"github.com/hashicorp/go-retryablehttp"
	json "github.com/json-iterator/go"
	"github.com/tidwall/gjson"

	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/logging"
	"github.com/infracost/infracost/internal/output"
	"github.com/infracost/infracost/internal/schema"
)

var jsonSorted = json.Config{SortMapKeys: true}.Froze()

type PolicyAPIClient struct {
	APIClient

	allowLists    map[string]allowList
	allowListErr  error
	allowListOnce sync.Once
}

// NewPolicyAPIClient retrieves resource allow-list info from Infracost Cloud and returns a new policy client
func NewPolicyAPIClient(ctx *config.RunContext) (*PolicyAPIClient, error) {
	client := retryablehttp.NewClient()
	client.Logger = &LeveledLogger{Logger: logging.Logger.With().Str("library", "retryablehttp").Logger()}
	c := PolicyAPIClient{
		APIClient: APIClient{
			httpClient: client.StandardClient(),
			endpoint:   ctx.Config.PolicyV2APIEndpoint,
			apiKey:     ctx.Config.APIKey,
			uuid:       ctx.UUID(),
		},
	}

	return &c, nil
}

type PolicyOutput struct {
	TagPolicies    []output.TagPolicy
	FinOpsPolicies []output.FinOpsPolicy
}

func (c *PolicyAPIClient) CheckPolicies(ctx *config.RunContext, out output.Root) (*PolicyOutput, error) {
	ri, err := newRunInput(ctx, out)
	if err != nil {
		return nil, err
	}

	q := `
		query($run: RunInput!) {
			evaluatePolicies(run: $run) {
				tagPolicyResults {
					name
					tagPolicyId
					message
					prComment
					blockPr
					totalDetectedResources
					totalTaggableResources
					resources {
						address
						resourceType
						path
						line
						projectNames
						missingMandatoryTags
						invalidTags {
							key
							value
							validValues
							validRegex
						}
					}
				}
				finopsPolicyResults {
					name
					policyId
					message
					blockPr
					prComment
					totalApplicableResources
					resources {
						checksum
						address
						resourceType
						path
						startLine
						endLine
						projectName
						issues {
							attribute
							value
							description
						}
						exclusionId
					}
				}
			}
		}
	`

	v := map[string]interface{}{
		"run": *ri,
	}
	results, err := c.doQueries([]GraphQLQuery{{q, v}})
	if err != nil {
		return nil, fmt.Errorf("query failed when checking tag policies %w", err)
	}

	if len(results) == 0 {
		return nil, nil
	}

	if results[0].Get("errors").Exists() {
		return nil, fmt.Errorf("query failed when checking tag policies, received graphql error: %s", results[0].Get("errors").String())
	}

	data := results[0].Get("data")

	var policies = struct {
		EvaluatePolicies struct {
			TagPolicies    []output.TagPolicy    `json:"tagPolicyResults"`
			FinOpsPolicies []output.FinOpsPolicy `json:"finopsPolicyResults"`
		} `json:"evaluatePolicies"`
	}{}

	err = json.Unmarshal([]byte(data.Raw), &policies)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal tag policies %w", err)
	}

	if len(policies.EvaluatePolicies.TagPolicies) > 0 {
		checkedStr := "tag policy"
		if len(policies.EvaluatePolicies.TagPolicies) > 1 {
			checkedStr = "tag policies"
		}
		msg := fmt.Sprintf(`%d %s checked`, len(policies.EvaluatePolicies.TagPolicies), checkedStr)
		if ctx.Config.IsLogging() {
			logging.Logger.Info().Msg(msg)
		} else {
			_, err := fmt.Fprintf(ctx.ErrWriter, "%s\n", msg)
			if err != nil {
				return nil, fmt.Errorf("failed to write tag policies %w", err)
			}
		}
	}

	if len(policies.EvaluatePolicies.FinOpsPolicies) > 0 {
		checkedStr := "finops policy"
		if len(policies.EvaluatePolicies.FinOpsPolicies) > 1 {
			checkedStr = "finops policies"
		}
		msg := fmt.Sprintf(`%d %s checked`, len(policies.EvaluatePolicies.FinOpsPolicies), checkedStr)
		if ctx.Config.IsLogging() {
			logging.Logger.Info().Msg(msg)
		} else {
			_, err := fmt.Fprintf(ctx.ErrWriter, "%s\n", msg)
			if err != nil {
				return nil, fmt.Errorf("failed to write fin ops policies %w", err)
			}
		}
	}

	return &PolicyOutput{policies.EvaluatePolicies.TagPolicies, policies.EvaluatePolicies.FinOpsPolicies}, nil
}

// UploadPolicyData sends a filtered set of a project's resource information to Infracost Cloud and
// potentially adds PolicySha and PastPolicySha to the project's metadata.
func (c *PolicyAPIClient) UploadPolicyData(project *schema.Project) error {
	if project.Metadata == nil {
		project.Metadata = &schema.ProjectMetadata{}
	}

	err := c.fetchAllowList()
	if err != nil {
		return err
	}

	filteredResources := c.filterResources(project.PartialResources)
	if len(filteredResources) > 0 {
		sha, err := c.uploadProjectPolicyData(filteredResources)
		if err != nil {
			return fmt.Errorf("failed to upload filtered partial resources %w", err)
		}
		project.Metadata.PolicySha = sha
	} else {
		project.Metadata.PolicySha = "0" // set a fake sha so we can tell policy checks actually ran
	}

	filteredPastResources := c.filterResources(project.PartialPastResources)
	if len(filteredPastResources) > 0 {
		sha, err := c.uploadProjectPolicyData(filteredPastResources)
		if err != nil {
			return fmt.Errorf("failed to upload filtered past partial resources %w", err)
		}
		project.Metadata.PastPolicySha = sha
	} else {
		project.Metadata.PastPolicySha = "0" // set a fake sha so we can tell policy checks actually ran
	}

	return nil
}

func (c *PolicyAPIClient) uploadProjectPolicyData(p2rs []policy2Resource) (string, error) {
	q := `
	mutation($policyResources: [PolicyResourceInput!]!) {
		storePolicyResources(policyResources: $policyResources) {
			sha
		}
	}
	`

	v := map[string]interface{}{
		"policyResources": p2rs,
	}

	results, err := c.doQueries([]GraphQLQuery{{q, v}})
	if err != nil {
		return "", fmt.Errorf("query storePolicyResources failed  %w", err)
	}

	if len(results) == 0 {
		return "", nil
	}

	if results[0].Get("errors").Exists() {
		return "", fmt.Errorf("query storePolicyResources failed, received graphql error: %s", results[0].Get("errors").String())
	}

	data := results[0].Get("data")

	var response struct {
		AddFilteredResourceSet struct {
			Sha string `json:"sha"`
		} `json:"storePolicyResources"`
	}

	err = json.Unmarshal([]byte(data.Raw), &response)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal storePolicyResources %w", err)
	}

	return response.AddFilteredResourceSet.Sha, nil
}

// graphql doesn't really like map/dictionary parameters, so convert tags,
// values, and refs to key/value arrays.

type policy2Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type policy2Reference struct {
	Key       string   `json:"key"`
	Addresses []string `json:"addresses"`
}

type policy2Resource struct {
	ResourceType string                   `json:"resourceType"`
	ProviderName string                   `json:"providerName"`
	Address      string                   `json:"address"`
	Tags         *[]policy2Tag            `json:"tags,omitempty"`
	Values       json.RawMessage          `json:"values"`
	References   []policy2Reference       `json:"references"`
	Metadata     policy2InfracostMetadata `json:"infracostMetadata"`
}

type policy2InfracostMetadata struct {
	Calls     []policy2InfracostMetadataCall `json:"calls"`
	Checksum  string                         `json:"checksum"`
	EndLine   int64                          `json:"endLine"`
	Filename  string                         `json:"filename"`
	StartLine int64                          `json:"startLine"`
}

type policy2InfracostMetadataCall struct {
	Filename  string `json:"filename"`
	BlockName string `json:"blockName"`
	StartLine int64  `json:"startLine"`
	EndLine   int64  `json:"endLine"`
}

func (c *PolicyAPIClient) filterResources(partials []*schema.PartialResource) []policy2Resource {
	var p2rs []policy2Resource
	for _, partial := range partials {
		if partial != nil && partial.ResourceData != nil {
			rd := partial.ResourceData
			if f, ok := c.allowLists[rd.Type]; ok {
				p2rs = append(p2rs, filterResource(rd, f))
			}
		}
	}

	sort.Slice(p2rs, func(i, j int) bool {
		return p2rs[i].Address < p2rs[j].Address
	})

	return p2rs
}

func filterResource(rd *schema.ResourceData, al allowList) policy2Resource {
	var tagsPtr *[]policy2Tag
	if rd.Tags != nil {
		tags := make([]policy2Tag, 0, len(*rd.Tags))
		for k, v := range *rd.Tags {
			tags = append(tags, policy2Tag{Key: k, Value: v})
		}
		sort.Slice(tags, func(i, j int) bool {
			return tags[i].Key < tags[j].Key
		})

		tagsPtr = &tags
	}

	// make sure the keys in the values json are sorted so we get consistent policyShas
	valuesJSON, err := jsonSorted.Marshal(filterValues(rd.RawValues, al))
	if err != nil {
		logging.Logger.Warn().Err(err).Str("address", rd.Address).Msg("Failed to marshal filtered values")
	}

	references := make([]policy2Reference, 0, len(rd.ReferencesMap))
	for k, refRds := range rd.ReferencesMap {
		refAddresses := make([]string, 0, len(refRds))
		for _, refRd := range refRds {
			refAddresses = append(refAddresses, refRd.Address)
		}
		references = append(references, policy2Reference{Key: k, Addresses: refAddresses})
	}
	sort.Slice(references, func(i, j int) bool {
		return references[i].Key < references[j].Key
	})

	var mdCalls []policy2InfracostMetadataCall
	for _, c := range rd.Metadata["calls"].Array() {
		mdCalls = append(mdCalls, policy2InfracostMetadataCall{
			BlockName: c.Get("blockName").String(),
			EndLine:   rd.Metadata["endLine"].Int(),
			Filename:  rd.Metadata["filename"].String(),
			StartLine: rd.Metadata["startLine"].Int(),
		})
	}

	checksum := rd.Metadata["checksum"].String()

	if checksum == "" {
		// this must be a plan json run.  calculate a checksum now.
		checksum = calcChecksum(rd)
	}

	return policy2Resource{
		ResourceType: rd.Type,
		ProviderName: rd.ProviderName,
		Address:      rd.Address,
		Tags:         tagsPtr,
		Values:       valuesJSON,
		References:   references,
		Metadata: policy2InfracostMetadata{
			Calls:     mdCalls,
			Checksum:  checksum,
			EndLine:   rd.Metadata["endLine"].Int(),
			Filename:  rd.Metadata["filename"].String(),
			StartLine: rd.Metadata["startLine"].Int(),
		},
	}
}

func filterValues(rd gjson.Result, allowList map[string]gjson.Result) map[string]interface{} {
	values := make(map[string]interface{}, len(allowList))
	for k, v := range rd.Map() {
		if allow, ok := allowList[k]; ok {
			if allow.IsBool() {
				if allow.Bool() {
					values[k] = json.RawMessage(v.Raw)
				}
			} else if allow.IsObject() {
				nestedAllow := allow.Map()
				if v.IsArray() {
					vArray := v.Array()
					nestedVals := make([]interface{}, 0, len(vArray))
					for _, el := range vArray {
						nestedVals = append(nestedVals, filterValues(el, nestedAllow))
					}
					values[k] = nestedVals
				} else {
					values[k] = filterValues(v, nestedAllow)
				}
			} else {
				logging.Logger.Warn().Str("Key", k).Str("Type", allow.Type.String()).Msg("Unknown allow type")
			}
		}
	}
	return values
}

func calcChecksum(rd *schema.ResourceData) string {
	h := sha256.New()
	h.Write([]byte(rd.ProviderName))
	h.Write([]byte(rd.Address))
	h.Write([]byte(rd.RawValues.Raw))

	return hex.EncodeToString(h.Sum(nil))
}

type allowList map[string]gjson.Result

func (c *PolicyAPIClient) fetchAllowList() error {
	c.allowListOnce.Do(func() {
		prw, err := c.getPolicyResourceAllowList()
		if err != nil {
			c.allowListErr = err
		}
		c.allowLists = prw
	})

	return c.allowListErr
}

func (c *PolicyAPIClient) getPolicyResourceAllowList() (map[string]allowList, error) {
	q := `
		query {
			policyResourceAllowList {
				resourceType
                allowed
			}
		}
	`
	v := map[string]interface{}{}

	results, err := c.doQueries([]GraphQLQuery{{q, v}})
	if err != nil {
		return nil, fmt.Errorf("query policyResourceAllowList failed %w", err)
	}

	if len(results) == 0 {
		return nil, nil
	}

	if results[0].Get("errors").Exists() {
		return nil, fmt.Errorf("query policyResourceAllowList failed, received graphql error: %s", results[0].Get("errors").String())
	}

	data := results[0].Get("data")

	var response struct {
		AllowLists []struct {
			Type    string          `json:"resourceType"`
			Allowed json.RawMessage `json:"allowed"`
		} `json:"policyResourceAllowList"`
	}

	err = json.Unmarshal([]byte(data.Raw), &response)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal policyResourceAllowList %w", err)
	}

	aw := map[string]allowList{}

	for _, rtf := range response.AllowLists {
		aw[rtf.Type] = gjson.ParseBytes(rtf.Allowed).Map()
	}

	return aw, nil
}
