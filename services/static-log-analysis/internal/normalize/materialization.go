package normalize

import (
	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

const (
	normalizedByteExpansion = int64(4)
	normalizedNodeOverhead  = int64(64)
)

func projectMaterialization(envelope model.TrustedEnvelope, request *collectorlogs.ExportLogsServiceRequest, maximum int64) (int64, error) {
	var projected int64
	envelopeWeight := normalizedNodeOverhead * int64(8+len(envelope.AllowedEnvironments)+len(envelope.AllowedServices))
	for _, text := range []string{
		string(envelope.SourceType), envelope.SourceAccount, envelope.Region, envelope.SourceInstance, envelope.CredentialIdentity,
	} {
		envelopeWeight += int64(len(text)) * normalizedByteExpansion
	}
	for _, text := range envelope.AllowedEnvironments {
		envelopeWeight += int64(len(text)) * normalizedByteExpansion
	}
	for _, text := range envelope.AllowedServices {
		envelopeWeight += int64(len(text)) * normalizedByteExpansion
	}

	for _, resourceLogs := range request.GetResourceLogs() {
		resourceWeight, err := attributesWeight(resourceLogs.GetResource().GetAttributes(), maximum)
		if err != nil {
			return 0, err
		}
		resourceRecords := int64(0)
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			scopeWeight, err := attributesWeight(scopeLogs.GetScope().GetAttributes(), maximum)
			if err != nil {
				return 0, err
			}
			scopeRecords := int64(len(scopeLogs.GetLogRecords()))
			resourceRecords += scopeRecords
			if err := addMaterializedProduct(&projected, scopeWeight, scopeRecords, maximum); err != nil {
				return 0, err
			}
			for _, record := range scopeLogs.GetLogRecords() {
				weight, err := recordWeight(record, maximum)
				if err != nil || addMaterialized(&projected, weight, maximum) != nil {
					return 0, ErrMaterializationTooLarge
				}
			}
		}
		if err := addMaterializedProduct(&projected, resourceWeight, resourceRecords, maximum); err != nil {
			return 0, err
		}
		if err := addMaterializedProduct(&projected, envelopeWeight, resourceRecords, maximum); err != nil {
			return 0, err
		}
	}
	return projected, nil
}

func recordWeight(record *logs.LogRecord, maximum int64) (int64, error) {
	weight := normalizedNodeOverhead + int64(len(record.GetSeverityText())+len(record.GetEventName())+len(record.GetTraceId())+len(record.GetSpanId()))*normalizedByteExpansion
	bodyWeight, err := anyValueWeight(record.GetBody(), maximum)
	if err != nil {
		return 0, err
	}
	if err := addMaterialized(&weight, bodyWeight, maximum); err != nil {
		return 0, err
	}
	attributes, err := attributesWeight(record.GetAttributes(), maximum)
	if err != nil || addMaterialized(&weight, attributes, maximum) != nil {
		return 0, ErrMaterializationTooLarge
	}
	return weight, nil
}

func attributesWeight(attributes []*common.KeyValue, maximum int64) (int64, error) {
	var weight int64
	for _, attribute := range attributes {
		entry := normalizedNodeOverhead + int64(len(attribute.GetKey()))*normalizedByteExpansion
		value, err := anyValueWeight(attribute.GetValue(), maximum)
		if err != nil || addMaterialized(&entry, value, maximum) != nil || addMaterialized(&weight, entry, maximum) != nil {
			return 0, ErrMaterializationTooLarge
		}
	}
	return weight, nil
}

func anyValueWeight(root *common.AnyValue, maximum int64) (int64, error) {
	if root == nil {
		return 0, nil
	}
	type item struct {
		value          *common.AnyValue
		containerDepth int
	}
	stack := []item{{value: root}}
	var weight int64
	nodes := 0
	scheduled := 1
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		nodes++
		if nodes > redact.DefaultMaxStructuredNodes || addMaterialized(&weight, normalizedNodeOverhead, maximum) != nil {
			return 0, ErrMaterializationTooLarge
		}
		switch value := current.value.GetValue().(type) {
		case *common.AnyValue_StringValue:
			if addMaterialized(&weight, int64(len(value.StringValue))*normalizedByteExpansion, maximum) != nil {
				return 0, ErrMaterializationTooLarge
			}
			if valueNodes, objectMembers, structured := redact.StructuredJSONShape(value.StringValue); structured {
				structuredOverhead := int64(valueNodes+objectMembers) * normalizedNodeOverhead
				if addMaterialized(&weight, structuredOverhead, maximum) != nil {
					return 0, ErrMaterializationTooLarge
				}
			}
		case *common.AnyValue_BytesValue:
			if addMaterialized(&weight, int64(len(value.BytesValue))*normalizedByteExpansion, maximum) != nil {
				return 0, ErrMaterializationTooLarge
			}
		case *common.AnyValue_ArrayValue:
			depth := current.containerDepth + 1
			if depth > redact.DefaultMaxStructuredDepth {
				return 0, ErrMaterializationTooLarge
			}
			children := value.ArrayValue.GetValues()
			if len(children) > redact.DefaultMaxStructuredNodes-scheduled {
				return 0, ErrMaterializationTooLarge
			}
			scheduled += len(children)
			for _, child := range children {
				stack = append(stack, item{value: child, containerDepth: depth})
			}
		case *common.AnyValue_KvlistValue:
			depth := current.containerDepth + 1
			if depth > redact.DefaultMaxStructuredDepth {
				return 0, ErrMaterializationTooLarge
			}
			children := value.KvlistValue.GetValues()
			if len(children) > redact.DefaultMaxStructuredNodes-scheduled {
				return 0, ErrMaterializationTooLarge
			}
			scheduled += len(children)
			for _, child := range children {
				if addMaterialized(&weight, normalizedNodeOverhead+int64(len(child.GetKey()))*normalizedByteExpansion, maximum) != nil {
					return 0, ErrMaterializationTooLarge
				}
				stack = append(stack, item{value: child.GetValue(), containerDepth: depth})
			}
		}
	}
	return weight, nil
}

func addMaterialized(total *int64, amount, maximum int64) error {
	if amount < 0 || *total > maximum-amount {
		return ErrMaterializationTooLarge
	}
	*total += amount
	return nil
}

func addMaterializedProduct(total *int64, amount, count, maximum int64) error {
	if count < 0 || amount < 0 || (count > 0 && amount > (maximum-*total)/count) {
		return ErrMaterializationTooLarge
	}
	return addMaterialized(total, amount*count, maximum)
}
