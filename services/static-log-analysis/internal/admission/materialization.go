package admission

const (
	materializedByteExpansion = int64(4)
	materializedNodeOverhead  = int64(64)
)

// projectMaterialization computes a conservative allocation/encoded-size
// weight directly over bounded OTLP wire messages. Shared resource and scope
// values are charged once per descendant record, preventing small wire payloads
// from expanding into unbounded per-record copies.
func projectMaterialization(data []byte, maximumDepth int, maximum int64) (int64, error) {
	var projected int64
	err := forEachMessage(data, 1, func(resourceLogs []byte) error {
		var resourceWeight int64
		var resourceRecords int64
		if err := forEachMessage(resourceLogs, 1, func(resource []byte) error {
			weight, err := resourceMaterializationWeight(resource, maximumDepth)
			if err != nil {
				return err
			}
			return addProjected(&resourceWeight, weight, maximum)
		}); err != nil {
			return err
		}
		if err := forEachMessage(resourceLogs, 2, func(scopeLogs []byte) error {
			var scopeWeight int64
			var scopeRecords int64
			if err := forEachMessage(scopeLogs, 1, func(scope []byte) error {
				weight, err := scopeMaterializationWeight(scope, maximumDepth)
				if err != nil {
					return err
				}
				return addProjected(&scopeWeight, weight, maximum)
			}); err != nil {
				return err
			}
			if err := forEachMessage(scopeLogs, 2, func(record []byte) error {
				scopeRecords++
				resourceRecords++
				weight, err := recordMaterializationWeight(record, maximumDepth)
				if err != nil {
					return err
				}
				return addProjected(&projected, weight, maximum)
			}); err != nil {
				return err
			}
			return addProjectedProduct(&projected, scopeWeight, scopeRecords, maximum)
		}); err != nil {
			return err
		}
		return addProjectedProduct(&projected, resourceWeight, resourceRecords, maximum)
	})
	if err != nil {
		if err == ErrMaterializationTooLarge {
			return 0, err
		}
		return 0, ErrMalformedRequest
	}
	return projected, nil
}

func resourceMaterializationWeight(data []byte, maximumDepth int) (int64, error) {
	budget := nodeBudget{maximum: DefaultMaxStructuralNodes}
	if err := countAttributes(data, 1, &budget, maximumDepth); err != nil {
		return 0, err
	}
	return weightedMessage(data, budget.used+1), nil
}

func scopeMaterializationWeight(data []byte, maximumDepth int) (int64, error) {
	budget := nodeBudget{maximum: DefaultMaxStructuralNodes}
	if err := countAttributes(data, 3, &budget, maximumDepth); err != nil {
		return 0, err
	}
	return weightedMessage(data, budget.used+1), nil
}

func recordMaterializationWeight(data []byte, maximumDepth int) (int64, error) {
	budget := nodeBudget{maximum: DefaultMaxStructuralNodes}
	if err := countLogRecord(data, &budget, maximumDepth); err != nil {
		return 0, err
	}
	return weightedMessage(data, budget.used+1), nil
}

func weightedMessage(data []byte, nodes int) int64 {
	return int64(len(data))*materializedByteExpansion + int64(nodes)*materializedNodeOverhead
}

func addProjected(total *int64, amount, maximum int64) error {
	if amount < 0 || *total > maximum-amount {
		return ErrMaterializationTooLarge
	}
	*total += amount
	return nil
}

func addProjectedProduct(total *int64, amount, count, maximum int64) error {
	if amount < 0 || count < 0 || (count > 0 && amount > (maximum-*total)/count) {
		return ErrMaterializationTooLarge
	}
	return addProjected(total, amount*count, maximum)
}
