package admission

import "google.golang.org/protobuf/encoding/protowire"

// sanitizeNesting inspects the small part of the OTLP wire schema that can
// recurse (AnyValue). Offending record-local submessages are replaced before
// generated protobuf decoding, so even extreme nesting remains a partial
// rejection and cannot exhaust the decoder stack.
func sanitizeNesting(data []byte, maximum int) ([]byte, []RecordRejection, []int, error) {
	index := 0
	var rejected []RecordRejection
	var acceptedOriginalIndexes []int
	sanitized, err := rewriteMessages(data, 1, func(resourceLogs []byte) ([]byte, error) {
		resourceBad, err := anyMessageFieldExceeds(resourceLogs, 1, func(resource []byte) (bool, error) {
			return attributesExceed(resource, 1, maximum)
		})
		if err != nil {
			return nil, err
		}
		return rewriteResourceLogs(resourceLogs, resourceBad, maximum, &index, &rejected, &acceptedOriginalIndexes)
	})
	return sanitized, rejected, acceptedOriginalIndexes, err
}

func rewriteResourceLogs(data []byte, resourceBad bool, maximum int, index *int, rejected *[]RecordRejection, acceptedOriginalIndexes *[]int) ([]byte, error) {
	withResource, err := rewriteMessages(data, 1, func(resource []byte) ([]byte, error) {
		if resourceBad {
			return nil, nil
		}
		return resource, nil
	})
	if err != nil {
		return nil, err
	}
	return rewriteMessages(withResource, 2, func(scopeLogs []byte) ([]byte, error) {
		scopeBad, err := anyMessageFieldExceeds(scopeLogs, 1, func(scope []byte) (bool, error) {
			return attributesExceed(scope, 3, maximum)
		})
		if err != nil {
			return nil, err
		}
		return rewriteScopeLogs(scopeLogs, resourceBad, scopeBad, maximum, index, rejected, acceptedOriginalIndexes)
	})
}

func rewriteScopeLogs(data []byte, resourceBad, scopeBad bool, maximum int, index *int, rejected *[]RecordRejection, acceptedOriginalIndexes *[]int) ([]byte, error) {
	withScope, err := rewriteMessages(data, 1, func(scope []byte) ([]byte, error) {
		if scopeBad {
			return nil, nil
		}
		return scope, nil
	})
	if err != nil {
		return nil, err
	}
	return rewriteMessagesFiltered(withScope, 2, func(record []byte) ([]byte, bool, error) {
		recordBad, err := logRecordExceeds(record, maximum)
		if err != nil {
			return nil, false, err
		}
		if resourceBad || scopeBad || recordBad {
			*rejected = append(*rejected, RecordRejection{Index: *index, Reason: ReasonNestingTooDeep})
			(*index)++
			return nil, false, nil
		}
		*acceptedOriginalIndexes = append(*acceptedOriginalIndexes, *index)
		(*index)++
		return record, true, nil
	})
}

func logRecordExceeds(data []byte, maximum int) (bool, error) {
	bodyBad, err := anyMessageFieldExceeds(data, 5, func(body []byte) (bool, error) {
		return anyValueExceeds(body, 1, maximum)
	})
	if err != nil || bodyBad {
		return bodyBad, err
	}
	return attributesExceed(data, 6, maximum)
}

func attributesExceed(data []byte, field protowire.Number, maximum int) (bool, error) {
	return anyMessageFieldExceeds(data, field, func(keyValue []byte) (bool, error) {
		return anyMessageFieldExceeds(keyValue, 2, func(value []byte) (bool, error) {
			return anyValueExceeds(value, 1, maximum)
		})
	})
}

func anyValueExceeds(data []byte, depth, maximum int) (bool, error) {
	if depth > maximum {
		return true, nil
	}
	return walkFields(data, func(number protowire.Number, wireType protowire.Type, value []byte) (bool, error) {
		if wireType != protowire.BytesType {
			return false, nil
		}
		switch number {
		case 5: // ArrayValue: repeated AnyValue values = 1.
			return anyMessageFieldExceeds(value, 1, func(child []byte) (bool, error) {
				return anyValueExceeds(child, depth+1, maximum)
			})
		case 6: // KeyValueList: repeated KeyValue values = 1; value = 2.
			return anyMessageFieldExceeds(value, 1, func(keyValue []byte) (bool, error) {
				return anyMessageFieldExceeds(keyValue, 2, func(child []byte) (bool, error) {
					return anyValueExceeds(child, depth+1, maximum)
				})
			})
		default:
			return false, nil
		}
	})
}

func anyMessageFieldExceeds(data []byte, field protowire.Number, check func([]byte) (bool, error)) (bool, error) {
	return walkFields(data, func(number protowire.Number, wireType protowire.Type, value []byte) (bool, error) {
		if number != field || wireType != protowire.BytesType {
			return false, nil
		}
		return check(value)
	})
}

func walkFields(data []byte, visit func(protowire.Number, protowire.Type, []byte) (bool, error)) (bool, error) {
	for len(data) > 0 {
		number, wireType, tagBytes := protowire.ConsumeTag(data)
		if tagBytes < 0 {
			return false, protowire.ParseError(tagBytes)
		}
		data = data[tagBytes:]
		valueBytes := protowire.ConsumeFieldValue(number, wireType, data)
		if valueBytes < 0 {
			return false, protowire.ParseError(valueBytes)
		}
		var value []byte
		if wireType == protowire.BytesType {
			var consumed int
			value, consumed = protowire.ConsumeBytes(data)
			if consumed < 0 {
				return false, protowire.ParseError(consumed)
			}
		}
		exceeds, err := visit(number, wireType, value)
		if err != nil || exceeds {
			return exceeds, err
		}
		data = data[valueBytes:]
	}
	return false, nil
}

func rewriteMessages(data []byte, field protowire.Number, rewrite func([]byte) ([]byte, error)) ([]byte, error) {
	out := make([]byte, 0, len(data))
	for len(data) > 0 {
		original := data
		number, wireType, tagBytes := protowire.ConsumeTag(data)
		if tagBytes < 0 {
			return nil, protowire.ParseError(tagBytes)
		}
		data = data[tagBytes:]
		valueBytes := protowire.ConsumeFieldValue(number, wireType, data)
		if valueBytes < 0 {
			return nil, protowire.ParseError(valueBytes)
		}
		if number == field && wireType == protowire.BytesType {
			message, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			replacement, err := rewrite(message)
			if err != nil {
				return nil, err
			}
			out = protowire.AppendTag(out, number, protowire.BytesType)
			out = protowire.AppendBytes(out, replacement)
		} else {
			out = append(out, original[:tagBytes+valueBytes]...)
		}
		data = data[valueBytes:]
	}
	return out, nil
}

// rewriteMessagesFiltered differs from rewriteMessages in one security-
// significant way: keep=false omits the repeated field entirely. Encoding an
// empty replacement would create a synthetic placeholder LogRecord that later
// stages could mistake for accepted input.
func rewriteMessagesFiltered(data []byte, field protowire.Number, rewrite func([]byte) ([]byte, bool, error)) ([]byte, error) {
	out := make([]byte, 0, len(data))
	for len(data) > 0 {
		original := data
		number, wireType, tagBytes := protowire.ConsumeTag(data)
		if tagBytes < 0 {
			return nil, protowire.ParseError(tagBytes)
		}
		data = data[tagBytes:]
		valueBytes := protowire.ConsumeFieldValue(number, wireType, data)
		if valueBytes < 0 {
			return nil, protowire.ParseError(valueBytes)
		}
		if number == field && wireType == protowire.BytesType {
			message, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			replacement, keep, err := rewrite(message)
			if err != nil {
				return nil, err
			}
			if keep {
				out = protowire.AppendTag(out, number, protowire.BytesType)
				out = protowire.AppendBytes(out, replacement)
			}
		} else {
			out = append(out, original[:tagBytes+valueBytes]...)
		}
		data = data[valueBytes:]
	}
	return out, nil
}
