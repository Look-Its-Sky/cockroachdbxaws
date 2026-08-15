// Package collectorconfig checks the shipped OpenTelemetry Collector
// configuration against the two properties the delivery contract rests on.
//
// The Collector is not part of this Go module and its configuration is not Go
// code, so nothing else would notice a change to it. But architecture.md makes
// the Collector queue the first of two durability layers, and security.md
// requires universal redaction to run before that queue exists on disk. A
// configuration that quietly lost either would be an unacknowledged change to
// what this service promises.
//
// There is no production code here.
package collectorconfig
