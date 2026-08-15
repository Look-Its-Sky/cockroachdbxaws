package model

// SafeValueKind discriminates a SafeValue. Untyped maps do not cross into
// domain logic, so every attribute value carries its kind explicitly.
type SafeValueKind string

const (
	// SafeKindEmpty is the zero value: no value is present at all.
	SafeKindEmpty  SafeValueKind = ""
	SafeKindString SafeValueKind = "string"
	SafeKindInt    SafeValueKind = "int"
	SafeKindDouble SafeValueKind = "double"
	SafeKindBool   SafeValueKind = "bool"
	SafeKindMap    SafeValueKind = "map"
	SafeKindSlice  SafeValueKind = "slice"
	// SafeKindWithheld replaces content that could not be safely parsed or
	// redacted. It carries a reason and never the original content.
	SafeKindWithheld SafeValueKind = "withheld"
)

// SafeValue is an attribute or body value that has passed redaction.
//
// The name records where in the pipeline a value belongs, not a property the
// type enforces. Its fields are exported and its constructors accept any
// string, so nothing prevents unredacted content from being placed in one.
// Redaction is enforced by the redaction stage, and persistence must still
// require redaction policy metadata and run final prohibited-content validation
// before storing or exporting a value, rather than trusting this type.
//
// Construct values through the helpers below rather than by literal, so that
// the kind and the payload cannot disagree.
type SafeValue struct {
	Kind     SafeValueKind
	String   string
	Int      int64
	Double   float64
	Bool     bool
	Map      map[string]SafeValue
	Slice    []SafeValue
	Withheld string
}

// SafeString returns a string value that has already passed redaction.
func SafeString(v string) SafeValue { return SafeValue{Kind: SafeKindString, String: v} }

// SafeInt returns an integer value.
func SafeInt(v int64) SafeValue { return SafeValue{Kind: SafeKindInt, Int: v} }

// SafeDouble returns a floating point value.
func SafeDouble(v float64) SafeValue { return SafeValue{Kind: SafeKindDouble, Double: v} }

// SafeBool returns a boolean value.
func SafeBool(v bool) SafeValue { return SafeValue{Kind: SafeKindBool, Bool: v} }

// SafeMap returns a structured value.
func SafeMap(v map[string]SafeValue) SafeValue { return SafeValue{Kind: SafeKindMap, Map: v} }

// SafeSlice returns a sequence value.
func SafeSlice(v ...SafeValue) SafeValue { return SafeValue{Kind: SafeKindSlice, Slice: v} }

// Withheld returns a placeholder for content that was removed. The reason is
// operator-facing metadata and must not embed the original content.
func Withheld(reason string) SafeValue { return SafeValue{Kind: SafeKindWithheld, Withheld: reason} }

// IsZero reports whether no value is present.
func (v SafeValue) IsZero() bool { return v.Kind == SafeKindEmpty }

// Depth returns the nesting depth of a value: 1 for a scalar, and one more than
// its deepest child for maps and slices. Admission uses this to bound structure
// before it is normalized.
func (v SafeValue) Depth() int {
	switch v.Kind {
	case SafeKindMap:
		deepest := 0
		for _, child := range v.Map {
			if d := child.Depth(); d > deepest {
				deepest = d
			}
		}
		return deepest + 1
	case SafeKindSlice:
		deepest := 0
		for _, child := range v.Slice {
			if d := child.Depth(); d > deepest {
				deepest = d
			}
		}
		return deepest + 1
	default:
		return 1
	}
}
