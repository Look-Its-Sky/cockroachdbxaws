// Package admission provides deterministic, storage-independent admission
// decisions. It does not mutate incident state, evaluate thresholds in event
// rules, persist records, or acknowledge transport requests.
package admission

import (
	"fmt"
	"strings"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

// Record is the small classification view admission needs. Event-rule state and
// log content are intentionally absent.
type Record struct {
	Source          model.SourceType
	Service         string
	Environment     string
	Severity        model.SeverityClass
	EventType       string
	Attributes      map[string]struct{}
	Security        bool
	EventTriggering bool
}

// Rule describes records one enabled rule could consume. Empty selectors are
// wildcards. RequiredAttributes are presence checks, not values.
type Rule struct {
	Sources            []model.SourceType
	Services           []string
	Environments       []string
	Severities         []model.SeverityClass
	EventTypes         []string
	RequiredAttributes []string
}

// Classifier is an immutable compiled rule set.
type Classifier struct{ rules []Rule }

// Classification makes uncertainty explicit. Only Protected=false and
// Certain=true is permission to consider shedding.
type Classification struct {
	Protected bool
	Certain   bool
}

// Compile validates and copies rules. A failed compile returns no classifier,
// whose Classify method still fails closed.
func Compile(rules []Rule) (*Classifier, error) {
	compiled := make([]Rule, len(rules))
	for i, rule := range rules {
		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("admission: rule %d: %w", i, err)
		}
		compiled[i] = cloneRule(rule)
	}
	return &Classifier{rules: compiled}, nil
}

func validateRule(rule Rule) error {
	for _, source := range rule.Sources {
		if source != model.SourceTypeOTLP && source != model.SourceTypeCloudWatch {
			return fmt.Errorf("unknown source type %q", source)
		}
	}
	for _, severity := range rule.Severities {
		switch severity {
		case model.SeverityClassUnspecified, model.SeverityClassTrace, model.SeverityClassDebug,
			model.SeverityClassInfo, model.SeverityClassWarn, model.SeverityClassError, model.SeverityClassFatal:
		default:
			return fmt.Errorf("unknown severity class %q", severity)
		}
	}
	for _, values := range [][]string{rule.Services, rule.Environments, rule.EventTypes, rule.RequiredAttributes} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("selector values must not be blank")
			}
		}
	}
	return nil
}

func cloneRule(rule Rule) Rule {
	rule.Sources = append([]model.SourceType(nil), rule.Sources...)
	rule.Services = append([]string(nil), rule.Services...)
	rule.Environments = append([]string(nil), rule.Environments...)
	rule.Severities = append([]model.SeverityClass(nil), rule.Severities...)
	rule.EventTypes = append([]string(nil), rule.EventTypes...)
	rule.RequiredAttributes = append([]string(nil), rule.RequiredAttributes...)
	return rule
}

// Classify reports whether any enabled rule could consume a record. Missing
// identity needed by a selector is uncertainty, so it is protected. A known
// selector mismatch proves that particular rule cannot consume the record.
func (c *Classifier) Classify(record Record) Classification {
	if c == nil {
		return Classification{Protected: true, Certain: false}
	}
	if !knownRecordEnums(record) {
		return Classification{Protected: true, Certain: false}
	}
	uncertain := false
	for _, rule := range c.rules {
		match, certain := couldMatch(rule, record)
		if match && certain {
			return Classification{Protected: true, Certain: true}
		}
		if match {
			uncertain = true
		}
	}
	if uncertain {
		return Classification{Protected: true, Certain: false}
	}
	return Classification{Protected: false, Certain: true}
}

func knownRecordEnums(record Record) bool {
	switch record.Source {
	case "":
	case model.SourceTypeOTLP, model.SourceTypeCloudWatch:
	default:
		return false
	}
	switch record.Severity {
	case "":
		return true
	case model.SeverityClassUnspecified, model.SeverityClassTrace, model.SeverityClassDebug,
		model.SeverityClassInfo, model.SeverityClassWarn, model.SeverityClassError, model.SeverityClassFatal:
		return true
	default:
		return false
	}
}

func couldMatch(rule Rule, record Record) (match, certain bool) {
	certain = true
	checks := []struct {
		value   string
		allowed []string
	}{
		{string(record.Source), sourceStrings(rule.Sources)},
		{record.Service, rule.Services},
		{record.Environment, rule.Environments},
		{string(record.Severity), severityStrings(rule.Severities)},
		{record.EventType, rule.EventTypes},
	}
	for _, check := range checks {
		if len(check.allowed) == 0 {
			continue
		}
		if check.value == "" {
			certain = false
			continue
		}
		if !hasString(check.allowed, check.value) {
			return false, true
		}
	}
	for _, key := range rule.RequiredAttributes {
		if _, ok := record.Attributes[key]; !ok {
			return false, true
		}
	}
	return true, certain
}

func sourceStrings(values []model.SourceType) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}

func severityStrings(values []model.SeverityClass) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
