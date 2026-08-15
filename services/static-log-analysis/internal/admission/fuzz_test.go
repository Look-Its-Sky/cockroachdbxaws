package admission_test

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
)

// testing.md requires Go fuzzing for OTLP parsing. Decode is the first thing an
// unauthenticated caller's bytes reach on the static_local socket, and every
// bound it enforces exists because the alternative is a decoder allocating
// whatever the caller asked for.
//
// The invariant is not "no panic". It is that Decode is total and that a
// success is always within the limits it was given: a result that exceeds them
// has already allocated the memory the bound existed to prevent.

func fuzzLimits() admission.Limits {
	// Deliberately small, so the fuzzer reaches the boundaries in short inputs
	// rather than needing to synthesize megabytes.
	return admission.Limits{
		MaxCompressedBytes: 4096, MaxUncompressedBytes: 16384, MaxRecords: 32,
		MaxNestingDepth: 4, MaxNormalizedBytes: 4096, MaxMaterializedBytes: 16384,
		MaxStructuralNodes: 256,
	}
}

func addDecodeSeeds(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		{0x00},
		{0xff, 0xff, 0xff, 0xff},
		// A minimal well-formed ExportLogsServiceRequest with one resource.
		{0x0a, 0x00},
		bytes.Repeat([]byte{0x0a, 0x02, 0x0a, 0x00}, 16),
		// Deep nesting, which is what the depth bound exists for.
		bytes.Repeat([]byte{0x0a, 0x02}, 64),
	}
	for _, seed := range seeds {
		f.Add(seed, false)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(bytes.Repeat([]byte{0x0a, 0x02, 0x0a, 0x00}, 64))
	_ = writer.Close()
	f.Add(compressed.Bytes(), true)
	// A gzip stream that expands far beyond the uncompressed bound: the shape a
	// decompression bomb takes.
	var bomb bytes.Buffer
	bombWriter := gzip.NewWriter(&bomb)
	_, _ = bombWriter.Write(bytes.Repeat([]byte{'A'}, 1<<20))
	_ = bombWriter.Close()
	f.Add(bomb.Bytes(), true)
}

// FuzzDecodeIsTotalAndNeverExceedsItsLimits runs the bounded decoder over
// arbitrary bytes in both encodings.
func FuzzDecodeIsTotalAndNeverExceedsItsLimits(f *testing.F) {
	addDecodeSeeds(f)
	limits := fuzzLimits()
	f.Fuzz(func(t *testing.T, payload []byte, useGzip bool) {
		encoding := admission.EncodingIdentity
		if useGzip {
			encoding = admission.EncodingGZIP
		}
		result, err := admission.Decode(payload, encoding, limits)
		if err != nil {
			// Refusing is always allowed; that is what the bounds are for.
			return
		}
		if result.Request == nil {
			t.Fatalf("Decode reported success with no request for %d bytes", len(payload))
		}
		if result.ProjectedMaterializedBytes > limits.MaxMaterializedBytes {
			t.Fatalf("accepted a request projected at %d bytes, over the %d byte limit",
				result.ProjectedMaterializedBytes, limits.MaxMaterializedBytes)
		}
		if result.ProjectedMaterializedBytes < 0 {
			t.Fatalf("projected materialization is negative: %d", result.ProjectedMaterializedBytes)
		}
		// The index mapping is what later stages use to report a partial
		// rejection against the original wire request. If it does not line up,
		// a rejection would name the wrong record.
		for _, index := range result.AcceptedOriginalIndexes {
			if index < 0 {
				t.Fatalf("accepted record maps to negative original index %d", index)
			}
		}
		for _, rejection := range result.Rejected {
			if rejection.Index < 0 {
				t.Fatalf("rejection names negative original index %d", rejection.Index)
			}
		}
	})
}

// FuzzDecodeNeverAcceptsMoreRecordsThanConfigured isolates the record-count
// bound, which is the one that decides how much work every later stage does.
func FuzzDecodeNeverAcceptsMoreRecordsThanConfigured(f *testing.F) {
	addDecodeSeeds(f)
	limits := fuzzLimits()
	f.Fuzz(func(t *testing.T, payload []byte, useGzip bool) {
		encoding := admission.EncodingIdentity
		if useGzip {
			encoding = admission.EncodingGZIP
		}
		result, err := admission.Decode(payload, encoding, limits)
		if err != nil {
			return
		}
		if got := len(result.AcceptedOriginalIndexes); got > limits.MaxRecords {
			t.Fatalf("accepted %d records, over the configured maximum of %d", got, limits.MaxRecords)
		}
	})
}
