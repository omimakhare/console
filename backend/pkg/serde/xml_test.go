// Copyright 2023 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

package serde

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestXMLSerde_DeserializePayload(t *testing.T) {
	serde := XMLSerde{}

	tests := []struct {
		name           string
		record         *kgo.Record
		payloadType    payloadType
		validationFunc func(t *testing.T, payload RecordPayload, err error)
	}{
		{
			name: "Valid XML Object in value",
			record: &kgo.Record{
				Value: []byte(`<?xml version="1.0" encoding="UTF-8"?><name>John</name><age>30</age>`),
			},
			payloadType: payloadTypeValue,
			validationFunc: func(t *testing.T, payload RecordPayload, err error) {
				require.NoError(t, err)
				assert.Nil(t, payload.Troubleshooting)
				assert.Nil(t, payload.SchemaID)
				assert.Equal(t, payloadEncodingXML, payload.Encoding)

				obj, ok := (payload.ParsedPayload).(map[string]any)
				require.Truef(t, ok, "parsed payload is not of type map[string]any")
				assert.Equal(t, "John", obj["name"])
				assert.EqualValues(t, "30", obj["age"])
			},
		},
		{
			name: "Valid XML Object in key",
			record: &kgo.Record{
				Key: []byte(`<?xml version="1.0" encoding="UTF-8"?><name>John</name><age>30</age>`),
			},
			payloadType: payloadTypeKey,
			validationFunc: func(t *testing.T, payload RecordPayload, err error) {
				require.NoError(t, err)
				assert.Nil(t, payload.Troubleshooting)
				assert.Nil(t, payload.SchemaID)
				assert.Equal(t, payloadEncodingXML, payload.Encoding)
			},
		},
		{
			name: "Invalid XML",
			record: &kgo.Record{
				Value: []byte(`this is no valid XML`),
			},
			payloadType: payloadTypeValue,
			validationFunc: func(t *testing.T, payload RecordPayload, err error) {
				assert.Error(t, err)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := serde.DeserializePayload(test.record, test.payloadType)
			test.validationFunc(t, payload, err)
		})
	}
}