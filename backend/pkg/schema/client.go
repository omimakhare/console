// Copyright 2022 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file https://github.com/redpanda-data/redpanda/blob/dev/licenses/bsl.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

package schema

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/redpanda-data/console/backend/pkg/config"
)

// Client that talks to the (Confluent) Schema Registry via REST
type Client struct {
	cfg    config.Schema
	client *resty.Client
}

// RestError represents the schema of the generic REST error that is returned
// for a failed HTTP request from the schema registry.
type RestError struct {
	ErrorCode int    `json:"error_code"`
	Message   string `json:"message"`
}

func (e RestError) Error() string {
	return fmt.Sprintf("schema registry request failed: %d - %s", e.ErrorCode, e.Message)
}

func newClient(cfg config.Schema) (*Client, error) {
	// TODO: Add support to fallback to other registry urls if provided
	registryURL := cfg.URLs[0] // Array length is checked in config validate()

	client := resty.New().
		SetBaseURL(registryURL).
		SetHeader("User-Agent", "Redpanda Console").
		SetHeader("Accept", "application/vnd.schemaregistry.v1+json").
		SetHeader("Content-Type", "application/vnd.schemaregistry.v1+json").
		SetError(&RestError{}).
		SetTimeout(5 * time.Second)

	// Configure credentials
	if cfg.Username != "" {
		client = client.SetBasicAuth(cfg.Username, cfg.Password)
	}
	if cfg.BearerToken != "" {
		client = client.SetAuthToken(cfg.BearerToken)
	}

	// Configure Client Certificate transport
	var caCertPool *x509.CertPool
	if cfg.TLS.Enabled {
		if cfg.TLS.CaFilepath != "" {
			ca, err := os.ReadFile(cfg.TLS.CaFilepath)
			if err != nil {
				return nil, err
			}
			caCertPool = x509.NewCertPool()
			isSuccessful := caCertPool.AppendCertsFromPEM(ca)
			if !isSuccessful {
				return nil, fmt.Errorf("failed to append ca file to cert pool, is this a valid PEM format?")
			}
		}

		// If configured load TLS cert & key - Mutual TLS
		var certificates []tls.Certificate
		if cfg.TLS.CertFilepath != "" && cfg.TLS.KeyFilepath != "" {
			cert, err := os.ReadFile(cfg.TLS.CertFilepath)
			if err != nil {
				return nil, fmt.Errorf("failed to read cert file for schema registry client: %w", err)
			}

			privateKey, err := os.ReadFile(cfg.TLS.KeyFilepath)
			if err != nil {
				return nil, fmt.Errorf("failed to read key file for schema registry client: %w", err)
			}

			pemBlock, _ := pem.Decode(privateKey)
			if pemBlock == nil {
				return nil, fmt.Errorf("no valid private key found")
			}

			tlsCert, err := tls.X509KeyPair(cert, privateKey)
			if err != nil {
				return nil, fmt.Errorf("failed to load certificate pair for schema registry client: %w", err)
			}
			certificates = []tls.Certificate{tlsCert}
		}

		transport := &http.Transport{TLSClientConfig: &tls.Config{
			//nolint:gosec // InsecureSkipVerify may be true upon user's responsibility.
			InsecureSkipVerify: cfg.TLS.InsecureSkipTLSVerify,
			Certificates:       certificates,
			RootCAs:            caCertPool,
		}}

		client.SetTransport(transport)
	}

	return &Client{
		cfg:    cfg,
		client: client,
	}, nil
}

// SchemaResponse is the schema of the GET /schemas/ids/${id} endpoint.
// `schema.Response` seems a little too vague for me.
//
//nolint:revive // This is stuttering when calling this with the pkg name, but without that the
type SchemaResponse struct {
	Schema     string      `json:"schema"`
	References []Reference `json:"references,omitempty"`
}

// Reference describes a reference to a different schema stored in the schema registry.
type Reference struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Version int    `json:"version"`
}

// GetSchemaByID returns the schema string identified by the input ID.
// id (int) – the globally unique identifier of the schema
func (c *Client) GetSchemaByID(ctx context.Context, id uint32) (*SchemaResponse, error) {
	url := fmt.Sprintf("/schemas/ids/%d", id)
	req := c.client.R().
		SetContext(ctx).
		SetResult(&SchemaResponse{})

	res, err := req.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get schema by id request failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get schema by id request failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	parsed, ok := res.Result().(*SchemaResponse)
	if !ok {
		return nil, fmt.Errorf("failed to parse schema response")
	}

	return parsed, nil
}

// SchemaVersionedResponse represents the schema resource returned by the Schema Registry
// `schema.VersionedResponse` seems a little too vague for me.
//
//nolint:revive // This is stuttering when calling this with the pkg name, but without that the
type SchemaVersionedResponse struct {
	Subject    string      `json:"subject"`
	SchemaID   int         `json:"id"`
	Version    int         `json:"version"`
	Schema     string      `json:"schema"`
	Type       string      `json:"schemaType"`
	References []Reference `json:"references"`
}

// GetSchemaBySubject returns the schema for the specified version of this subject. The unescaped schema only is returned.
// subject (string) – Name of the subject
// version (versionId) – Version of the schema to be returned. Valid values for versionId are between [1,2^31-1] or
//
//	the string “latest”, which returns the last registered schema under the specified subject.
//	Note that there may be a new latest schema that gets registered right after this request is served.
func (c *Client) GetSchemaBySubject(ctx context.Context, subject, version string, showSoftDeleted bool) (*SchemaVersionedResponse, error) {
	req := c.client.R().
		SetContext(ctx).
		SetResult(&SchemaVersionedResponse{}).
		SetPathParams(map[string]string{
			"subjects": subject,
			"version":  version,
		})
	if showSoftDeleted {
		req.SetQueryParam("deleted", "true")
	}

	res, err := req.Get("/subjects/{subjects}/versions/{version}")
	if err != nil {
		return nil, fmt.Errorf("get schema by subject request failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get schema by subject request failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	parsed, ok := res.Result().(*SchemaVersionedResponse)
	if !ok {
		return nil, fmt.Errorf("failed to parse schema by subject response")
	}
	if parsed.Type == "" {
		parsed.Type = TypeAvro.String()
	}

	return parsed, nil
}

// SubjectsResponse is the schema for the GET /subjects endpoint.
type SubjectsResponse struct {
	Subjects []string // Subject names
}

// GetSubjects returns a list of registered subjects.
func (c *Client) GetSubjects(ctx context.Context, showSoftDeleted bool) (*SubjectsResponse, error) {
	req := c.client.R().
		SetContext(ctx).
		SetResult([]string{})

	if showSoftDeleted {
		req.SetQueryParam("deleted", "true")
	}

	res, err := req.Get("/subjects")
	if err != nil {
		return nil, fmt.Errorf("get subjects request failed: %w", err)
	}

	result := res.Result()
	parsed, ok := result.(*[]string)
	if !ok {
		return nil, fmt.Errorf("failed to parse subjects response")
	}

	return &SubjectsResponse{
		Subjects: *parsed,
	}, nil
}

// SubjectVersionsResponse is the response schema of the GET `/subjects/{subject}/versions`
// endpoint.
type SubjectVersionsResponse struct {
	Versions []int
}

// GetSubjectVersions returns a schema subject's registered versions.
func (c *Client) GetSubjectVersions(ctx context.Context, subject string, showSoftDeleted bool) (*SubjectVersionsResponse, error) {
	req := c.client.R().
		SetContext(ctx).
		SetResult([]int{}).
		SetPathParam("subject", subject)

	if showSoftDeleted {
		req.SetQueryParam("deleted", "true")
	}

	res, err := req.Get("/subjects/{subject}/versions")
	if err != nil {
		return nil, fmt.Errorf("get subject versions request failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get subject versions request failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	parsed, ok := res.Result().(*[]int)
	if !ok {
		return nil, fmt.Errorf("failed to parse subject versions response")
	}

	return &SubjectVersionsResponse{
		Versions: *parsed,
	}, nil
}

// ModeResponse is the schema of the GET /mode endpoint.
type ModeResponse struct {
	// Possible values are: IMPORT, READONLY, READWRITE
	Mode string `json:"mode"`
}

// GetMode returns the current mode for Schema Registry at a global level.
func (c *Client) GetMode(ctx context.Context) (*ModeResponse, error) {
	res, err := c.client.R().
		SetContext(ctx).
		SetResult(&ModeResponse{}).
		Get("/mode")
	if err != nil {
		return nil, fmt.Errorf("get mode request failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get mode request failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	parsed, ok := res.Result().(*ModeResponse)
	if !ok {
		return nil, fmt.Errorf("failed to parse mode response")
	}

	return parsed, nil
}

// ConfigResponse is the response schema for the schema registry's /config endpoint.
type ConfigResponse struct {
	// Global compatibility level. Will be one of:
	// BACKWARD, BACKWARD_TRANSITIVE, FORWARD, FORWARD_TRANSITIVE, FULL, FULL_TRANSITIVE, NONE, DEFAULT (only for subject configs)
	Compatibility string `json:"compatibilityLevel"`
}

// GetConfig gets global compatibility level.
func (c *Client) GetConfig(ctx context.Context) (*ConfigResponse, error) {
	res, err := c.client.R().
		SetContext(ctx).
		SetResult(&ConfigResponse{}).
		Get("/config")
	if err != nil {
		return nil, fmt.Errorf("get config failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get config failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	parsed, ok := res.Result().(*ConfigResponse)
	if !ok {
		return nil, fmt.Errorf("failed to parse config response")
	}

	return parsed, nil
}

// GetSubjectConfig gets compatibility level for a given subject.
// If the subject you ask about does not have a subject-specific compatibility level set, this command returns an
// error code. For example, if you run the same command for the subject Kafka-value, for which you have not set
// subject-specific compatibility, you get: {"error_code":40401,"message":"Subject 'Kafka-value' not found."}
func (c *Client) GetSubjectConfig(ctx context.Context, subject string) (*ConfigResponse, error) {
	res, err := c.client.R().
		SetContext(ctx).
		SetResult(&ConfigResponse{}).
		SetPathParam("subject", subject).
		SetPathParam("defaultToGlobal", "true").
		Get("/config/{subject}")
	if err != nil {
		return nil, fmt.Errorf("get config for subject failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get config for subject failed: Status code %d", res.StatusCode())
		}

		if restErr.ErrorCode == CodeSubjectNotFound {
			return &ConfigResponse{
				Compatibility: "DEFAULT",
			}, nil
		}
		return nil, restErr
	}

	parsed, ok := res.Result().(*ConfigResponse)
	if !ok {
		return nil, fmt.Errorf("failed to parse config for subject response")
	}

	return parsed, nil
}

// DeleteSubjectResponse describes the response to deleting a whole subject.
type DeleteSubjectResponse struct {
	Versions []int
}

// DeleteSubject deletes the specified subject and its associated compatibility level if registered.
// If deletePermanently is set to true, a hard delete will be sent which removes all the associated
// metadata including the schema ids that belong to this subject. To perform a hard-delete you must
// soft-delete the subject first.
func (c *Client) DeleteSubject(ctx context.Context, subject string, deletePermanently bool) (*DeleteSubjectResponse, error) {
	var deletedVersions []int
	res, err := c.client.R().
		SetContext(ctx).
		SetResult(&deletedVersions).
		SetPathParam("subject", subject).
		SetQueryParam("permanent", strconv.FormatBool(deletePermanently)).
		Delete("/subjects/{subject}")
	if err != nil {
		return nil, fmt.Errorf("delete subject failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("delete subject failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	return &DeleteSubjectResponse{deletedVersions}, nil
}

// DeleteSubjectVersionResponse describes the response to deleting a subject version.
type DeleteSubjectVersionResponse struct {
	Version int
}

// DeleteSubjectVersion deletes a specific version of the subject. Unless you delete permanently,
// this only deletes the version, leaving the schema ID intact and making it still possible to
// decode data using the schema ID.
func (c *Client) DeleteSubjectVersion(ctx context.Context, subject, version string, deletePermanently bool) (*DeleteSubjectVersionResponse, error) {
	var deletedVersion int
	res, err := c.client.R().
		SetContext(ctx).
		SetResult(&deletedVersion).
		SetPathParam("subject", subject).
		SetPathParam("version", version).
		SetQueryParam("permanent", strconv.FormatBool(deletePermanently)).
		Delete("/subjects/{subject}/versions/{version}")
	if err != nil {
		return nil, fmt.Errorf("delete subject version failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("delete subject version failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	return &DeleteSubjectVersionResponse{deletedVersion}, nil
}

// GetSchemaTypes returns supported types (AVRO, PROTOBUF, JSON)
func (c *Client) GetSchemaTypes(ctx context.Context) ([]string, error) {
	var supportedTypes []string
	req := c.client.R().
		SetContext(ctx).
		SetResult(&supportedTypes)

	res, err := req.Get("/schemas/types")
	if err != nil {
		return nil, fmt.Errorf("get schema types failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get schema types failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	return supportedTypes, nil
}

// GetSchemas retrieves all stored schemas from a schema registry.
func (c *Client) GetSchemas(ctx context.Context, showSoftDeleted bool) ([]SchemaVersionedResponse, error) {
	var schemas []SchemaVersionedResponse
	req := c.client.R().
		SetContext(ctx).
		SetResult(&schemas)

	if showSoftDeleted {
		req.SetQueryParam("deleted", "true")
	}

	res, err := req.Get("/schemas")
	if err != nil {
		return nil, fmt.Errorf("get schemas failed: %w", err)
	}

	if res.StatusCode() == http.StatusNotFound {
		// The /schemas endpoint has been introduced with v6.0.0, so instead we could achieve the same by querying
		// every subject one by one
		return c.GetSchemasIndividually(ctx, showSoftDeleted)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("get schemas failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	return schemas, nil
}

// Schema is the object form of a schema for the HTTP API.
type Schema struct {
	// Schema is the actual unescaped text of a schema.
	Schema string `json:"schema"`

	// Type is the type of a schema. The default type is avro.
	Type SchemaType `json:"schemaType,omitempty"`

	// References declares other schemas this schema references. See the
	// docs on SchemaReference for more details.
	References []SchemaReference `json:"references,omitempty"`
}

// SchemaReference is a way for a one schema to reference another. The details
// for how referencing is done are type specific; for example, JSON objects
// that use the key "$ref" can refer to another schema via URL. For more details
// on references, see the following link:
//
//	https://docs.confluent.io/platform/current/schema-registry/serdes-develop/index.html#schema-references
//	https://docs.confluent.io/platform/current/schema-registry/develop/api.html
//
//nolint:revive // The name reference would be too generic in this case.
type SchemaReference struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Version int    `json:"version"`
}

// CreateSchemaResponse is the response to creating a schema.
type CreateSchemaResponse struct {
	ID int `json:"id"`
}

// CreateSchema registers a new schema under the specified subject.
func (c *Client) CreateSchema(ctx context.Context, subjectName string, schema Schema) (*CreateSchemaResponse, error) {
	var createSchemaRes CreateSchemaResponse
	res, err := c.client.R().
		SetContext(ctx).
		SetResult(&createSchemaRes).
		SetPathParam("subject", subjectName).
		SetQueryParam("normalize", "true").
		SetBody(&schema).
		Post("/subjects/{subject}/versions")
	if err != nil {
		return nil, fmt.Errorf("create schema failed: %w", err)
	}

	if res.IsError() {
		restErr, ok := res.Error().(*RestError)
		if !ok {
			return nil, fmt.Errorf("create schema failed: Status code %d", res.StatusCode())
		}
		return nil, restErr
	}

	return &createSchemaRes, nil
}

// GetSchemasIndividually returns all schemas by describing all schemas one by one. This may be used against
// schema registry that don't support the /schemas endpoint that returns a list of all registered schemas.
func (c *Client) GetSchemasIndividually(ctx context.Context, showSoftDeleted bool) ([]SchemaVersionedResponse, error) {
	subjectsRes, err := c.GetSubjects(ctx, showSoftDeleted)
	if err != nil {
		return nil, fmt.Errorf("failed to get subjects to fetch schemas for: %w", err)
	}

	type chRes struct {
		schemaRes *SchemaVersionedResponse
		err       error
	}
	ch := make(chan chRes, len(subjectsRes.Subjects))

	// Describe all subjects concurrently one by one
	for _, subject := range subjectsRes.Subjects {
		go func(s string) {
			r, err := c.GetSchemaBySubject(ctx, s, "latest", showSoftDeleted)
			ch <- chRes{
				schemaRes: r,
				err:       err,
			}
		}(subject)
	}

	schemas := make([]SchemaVersionedResponse, 0)
	for i := 0; i < cap(ch); i++ {
		res := <-ch
		if res.err != nil {
			return nil, fmt.Errorf("failed to fetch at least one schema: %w", res.err)
		}
		schemas = append(schemas, *res.schemaRes)
	}

	return schemas, nil
}

// CheckConnectivity checks whether the schema registry can be access by GETing the /subjects
func (c *Client) CheckConnectivity(ctx context.Context) error {
	url := "subjects"
	res, err := c.client.R().SetContext(ctx).Get(url)
	if err != nil {
		return err
	}

	if res.IsError() {
		body := string(res.Body())
		return fmt.Errorf("response is an error. Status: %d - %s", res.StatusCode(), body)
	}

	return nil
}
