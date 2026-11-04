package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"go.yaml.in/yaml/v3"
)

// This harness publishes through the actual Configuration owner. It does not
// implement or register Dashboard RPCs, and never reads editable files on behalf
// of a dashboard caller. Authorization and projection remain in production.
type nhf005Publication struct {
	client              connect.HTTPClient
	origin, token, home string
	previous            map[string]*api.ConfigurationDocument
	raw                 map[string][]byte
}

func nhf005Publisher(t *testing.T, ctx context.Context, client connect.HTTPClient, origin, token string) *nhf005Publication {
	t.Helper()
	return &nhf005Publication{client: client, origin: origin, token: token, home: nha203PublicHomeName(t, ctx, client, origin, token), previous: make(map[string]*api.ConfigurationDocument), raw: make(map[string][]byte)}
}

func (p *nhf005Publication) document(ctx context.Context, identity string) (*api.ConfigurationDocument, error) {
	service := apiv2connect.NewConfigurationServiceClient(p.client, p.origin)
	cursor := ""
	for page := 0; page < 32; page++ {
		response, err := service.ListConfigurationDocuments(ctx, nha056SessionRequest(&api.ListConfigurationDocumentsRequest{Parent: p.home, PageSize: 100, PageToken: cursor}, p.token))
		if err != nil {
			return nil, err
		}
		for _, document := range response.Msg.Documents {
			if document.GetId().GetValue() == identity {
				read, err := service.GetConfigurationDocument(ctx, nha056SessionRequest(&api.GetConfigurationDocumentRequest{Name: document.Name}, p.token))
				if err != nil {
					return nil, err
				}
				return read.Msg, nil
			}
		}
		cursor = response.Msg.NextPageToken
		if cursor == "" {
			break
		}
	}
	return nil, errors.New("native fixture document not found")
}

func (p *nhf005Publication) publish(ctx context.Context, identity, file, owner, urlPath, routePrefix string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("dashboard fixture exceeds source bound")
	}
	var document yaml.Node
	if err = yaml.Unmarshal(raw, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("dashboard fixture requires a mapping")
	}
	root := document.Content[0]
	var metadata *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "homeward" {
			metadata = root.Content[i+1]
			break
		}
	}
	if metadata == nil {
		metadata = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "homeward"}, metadata)
	}
	if metadata.Kind != yaml.MappingNode {
		return errors.New("dashboard fixture metadata must be a mapping")
	}
	set := func(key, value string) {
		for i := 0; i < len(metadata.Content); i += 2 {
			if metadata.Content[i].Value == key {
				metadata.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
				return
			}
		}
		metadata.Content = append(metadata.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	// These values are trusted fixture configuration, never request-derived owners.
	set("owner", owner)
	set("url_path", urlPath)
	hasProfile := false
	for i := 0; i < len(metadata.Content); i += 2 {
		hasProfile = hasProfile || metadata.Content[i].Value == "profile"
	}
	if !hasProfile {
		set("profile", "homeward-dashboard-2")
	}
	if routePrefix != "" {
		var rewrite func(*yaml.Node)
		rewrite = func(node *yaml.Node) {
			if node.Kind == yaml.MappingNode {
				for i := 0; i < len(node.Content); i += 2 {
					key, value := node.Content[i], node.Content[i+1]
					if key.Value == "navigation_path" && value.Kind == yaml.ScalarNode && strings.HasPrefix(value.Value, "/") && !strings.HasPrefix(value.Value, "//") {
						value.Value = "/" + routePrefix + strings.TrimPrefix(value.Value, "/")
					}
					rewrite(value)
				}
			} else {
				for _, child := range node.Content {
					rewrite(child)
				}
			}
		}
		rewrite(root)
	}
	raw, err = yaml.Marshal(&document)
	if err != nil {
		return err
	}
	if bytes.Equal(p.raw[identity], raw) {
		return nil
	}
	before, err := p.document(ctx, "operational.main")
	if err != nil {
		return err
	}
	entry := filepath.Base(file)
	candidate := &api.ConfigurationPlanCandidate{Family: api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: identity, Entry: entry, Files: []*api.ConfigurationPlanSourceFile{{LogicalPath: entry, ExactSource: raw}}}
	plans := apiv2connect.NewConfigurationPlanServiceClient(p.client, p.origin)
	plan, err := plans.CreateConfigurationPlan(ctx, nha056SessionRequest(&api.CreateConfigurationPlanRequest{Parent: p.home, ConfigurationPlan: &api.ConfigurationPlan{Candidate: candidate}}, p.token))
	if err != nil {
		return fmt.Errorf("dashboard plan: %w", err)
	}
	request := nha055Request(before, candidate, plan.Msg)
	var previous *api.DocumentVersion
	if prior := p.previous[identity]; prior != nil {
		previous = prior.Version
	}
	request.Sources = []*api.SourcePrecondition{nha055PublicationExpected(identity, entry, previous, p.raw[identity])}
	result, err := apiv2connect.NewConfigurationApplyServiceClient(p.client, p.origin).ApplyConfigurationPlan(ctx, nha056SessionRequest(request, p.token))
	if err != nil {
		return fmt.Errorf("dashboard publication: %w", err)
	}
	if !result.Msg.Done || result.Msg.GetError() != nil {
		return errors.New("dashboard publication did not complete successfully")
	}
	current, err := p.document(ctx, identity)
	if err != nil {
		return err
	}
	p.previous[identity], p.raw[identity] = current, bytes.Clone(raw)
	return nil
}
