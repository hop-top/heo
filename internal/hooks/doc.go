// Package hooks runs hook plugins: one durable uma subscription per
// plugin, each event handed to the plugin as an envelope of references,
// the plugin's result applied, and the subscription's cursor advanced
// only once every event of the batch was applied or dead-lettered.
//
// # Ordering and isolation
//
// A Runner is one plugin's alone: its own subscription, its own
// goroutines, its own failures. Nothing is shared between runners and
// nothing a plugin does, however slow, holds a worker, a receiver or
// the bridge.
//
// Within a batch, events of one conversation (the resource when an
// event names none) are delivered one at a time in log order; events of
// different conversations run in parallel up to the plugin's
// concurrency. A stream plugin is one ordered process: concurrency 1.
//
// # Cursor rule
//
// The batch is acknowledged after each of its events was applied, or
// recorded as dead-lettered once the plugin failed for good or ran out
// of attempts. A failure to record a dead letter, to apply, to shut the
// runner down mid-batch: none acknowledges, and the batch is redelivered
// (at least once; application is idempotent).
package hooks
