package cwcheckpoint_test

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwcheckpoint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/golden"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/pebbletest"
)

func testGroup() cloudwatch.Group {
	return cloudwatch.Group{Account: "123456789012", Region: "us-east-1", LogGroup: "/aws/ecs/paymentservice"}
}

func streamIn(group cloudwatch.Group, name string) cloudwatch.Stream {
	return cloudwatch.Stream{Group: group, Name: name}
}

func checkpointAt(stream cloudwatch.Stream, position time.Time) cloudwatch.Checkpoint {
	return cloudwatch.Checkpoint{
		Stream:      stream,
		Position:    position,
		EventID:     "39518522779705548990893699571816765783291046441449848832",
		CommittedAt: position.Add(time.Second),
	}
}

func newStore(t *testing.T) (*cwcheckpoint.Store, *pebbletest.Store) {
	t.Helper()
	pebble := pebbletest.New(t)
	store, err := cwcheckpoint.New(pebble.DB())
	if err != nil {
		t.Fatalf("opening a checkpoint store: %v", err)
	}
	return store, pebble
}

func TestACommittedCheckpointIsReadBack(t *testing.T) {
	store, _ := newStore(t)
	group := testGroup()
	position := fakeclock.Origin.Add(5 * time.Minute)

	if err := store.Commit(context.Background(), []cloudwatch.Checkpoint{
		checkpointAt(streamIn(group, "ecs/paymentservice/0e1f"), position),
	}); err != nil {
		t.Fatalf("committing: %v", err)
	}

	loaded, err := store.LoadGroup(context.Background(), group)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("want one checkpoint, got %d", len(loaded))
	}
	if !loaded[0].Position.Equal(position) {
		t.Fatalf("want position %s, got %s", position, loaded[0].Position)
	}
	if loaded[0].Stream.Name != "ecs/paymentservice/0e1f" {
		t.Fatalf("a checkpoint must reload knowing which stream it belongs to, got %q", loaded[0].Stream.Name)
	}
	if loaded[0].Stream.Group != group {
		t.Fatalf("want group %+v, got %+v", group, loaded[0].Stream.Group)
	}
}

// A checkpoint is the one piece of adapter state a restart depends on. If it
// does not survive a power loss, the adapter rereads from its bounded floor,
// which is safe but costs a full lookback of duplicate delivery every restart.
func TestACommittedCheckpointSurvivesACrash(t *testing.T) {
	pebble := pebbletest.NewCrashable(t)
	store, err := cwcheckpoint.New(pebble.DB())
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	group := testGroup()
	position := fakeclock.Origin.Add(9 * time.Minute)
	if err := store.Commit(context.Background(), []cloudwatch.Checkpoint{
		checkpointAt(streamIn(group, "ecs/paymentservice/0e1f"), position),
	}); err != nil {
		t.Fatalf("committing: %v", err)
	}

	pebble.CrashAndReopen(t)

	reopened, err := cwcheckpoint.New(pebble.DB())
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	loaded, err := reopened.LoadGroup(context.Background(), group)
	if err != nil {
		t.Fatalf("loading after a crash: %v", err)
	}
	if len(loaded) != 1 || !loaded[0].Position.Equal(position) {
		t.Fatalf("a committed checkpoint did not survive a crash: %+v", loaded)
	}
}

func TestLoadGroupReturnsOnlyThatGroupsStreams(t *testing.T) {
	store, _ := newStore(t)
	group := testGroup()
	other := group
	other.LogGroup = "/aws/ecs/cartservice"
	otherRegion := group
	otherRegion.Region = "eu-west-1"
	otherAccount := group
	otherAccount.Account = "210987654321"

	position := fakeclock.Origin
	if err := store.Commit(context.Background(), []cloudwatch.Checkpoint{
		checkpointAt(streamIn(group, "a"), position),
		checkpointAt(streamIn(group, "b"), position),
		checkpointAt(streamIn(other, "a"), position),
		checkpointAt(streamIn(otherRegion, "a"), position),
		checkpointAt(streamIn(otherAccount, "a"), position),
	}); err != nil {
		t.Fatalf("committing: %v", err)
	}

	loaded, err := store.LoadGroup(context.Background(), group)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("want only this group's two streams, got %d: %+v", len(loaded), loaded)
	}
	for _, checkpoint := range loaded {
		if checkpoint.Stream.Group != group {
			t.Fatalf("a foreign group leaked into the answer: %+v", checkpoint.Stream.Group)
		}
	}
}

// A key built by joining free-form names with a separator lets an operator move
// a boundary by naming a group with the separator in it. Two distinct streams
// would then share a checkpoint, and one of them would silently skip records.
func TestKeysCannotBeConfusedByAComponentContainingTheSeparator(t *testing.T) {
	store, _ := newStore(t)
	base := testGroup()
	left := base
	left.LogGroup = "/aws/ecs/pay"
	right := base
	right.LogGroup = "/aws/ecs"

	early := fakeclock.Origin
	late := fakeclock.Origin.Add(time.Hour)
	if err := store.Commit(context.Background(), []cloudwatch.Checkpoint{
		checkpointAt(streamIn(left, "/service"), early),
		checkpointAt(streamIn(right, "/pay/service"), late),
	}); err != nil {
		t.Fatalf("committing: %v", err)
	}

	loadedLeft, err := store.LoadGroup(context.Background(), left)
	if err != nil {
		t.Fatalf("loading left: %v", err)
	}
	if len(loadedLeft) != 1 || !loadedLeft[0].Position.Equal(early) {
		t.Fatalf("one stream overwrote another's checkpoint: %+v", loadedLeft)
	}
}

func TestRecommittingAStreamReplacesItsPosition(t *testing.T) {
	store, _ := newStore(t)
	group := testGroup()
	stream := streamIn(group, "ecs/paymentservice/0e1f")

	for _, minutes := range []int{1, 7, 4} {
		if err := store.Commit(context.Background(), []cloudwatch.Checkpoint{
			checkpointAt(stream, fakeclock.Origin.Add(time.Duration(minutes)*time.Minute)),
		}); err != nil {
			t.Fatalf("committing: %v", err)
		}
	}

	loaded, err := store.LoadGroup(context.Background(), group)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	// The store records what it is told. Deciding that a position may only move
	// forward belongs to the adapter, which alone knows what was acknowledged.
	if len(loaded) != 1 || !loaded[0].Position.Equal(fakeclock.Origin.Add(4*time.Minute)) {
		t.Fatalf("want the last committed position, got %+v", loaded)
	}
}

func TestAnUnusableCheckpointIsRefusedRatherThanStored(t *testing.T) {
	store, _ := newStore(t)
	group := testGroup()

	tests := []struct {
		name       string
		checkpoint cloudwatch.Checkpoint
	}{
		{"no stream name", checkpointAt(streamIn(group, ""), fakeclock.Origin)},
		{"no group", checkpointAt(streamIn(cloudwatch.Group{}, "a"), fakeclock.Origin)},
		{"zero position", cloudwatch.Checkpoint{Stream: streamIn(group, "a"), CommittedAt: fakeclock.Origin}},
		{"zero commit time", cloudwatch.Checkpoint{Stream: streamIn(group, "a"), Position: fakeclock.Origin}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.Commit(context.Background(), []cloudwatch.Checkpoint{test.checkpoint}); err == nil {
				t.Fatal("want a refusal rather than an unreadable checkpoint on disk")
			}
		})
	}

	loaded, err := store.LoadGroup(context.Background(), group)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("a refused commit must leave nothing behind, got %+v", loaded)
	}
}

// A commit is one batch. Half a cycle's checkpoints would leave the other half
// rereading an overlap the adapter believed it had passed.
func TestACommitIsAllOrNothing(t *testing.T) {
	store, _ := newStore(t)
	group := testGroup()

	err := store.Commit(context.Background(), []cloudwatch.Checkpoint{
		checkpointAt(streamIn(group, "good"), fakeclock.Origin),
		checkpointAt(streamIn(group, ""), fakeclock.Origin),
	})
	if err == nil {
		t.Fatal("want the batch refused")
	}

	loaded, loadErr := store.LoadGroup(context.Background(), group)
	if loadErr != nil {
		t.Fatalf("loading: %v", loadErr)
	}
	if len(loaded) != 0 {
		t.Fatalf("a refused batch wrote part of itself: %+v", loaded)
	}
}

// The key encoding is a persistence contract: changing it silently orphans
// every checkpoint in every deployment, and the adapter then rereads from its
// floor with no error anywhere to say why.
func TestCheckpointKeyEncodingIsFixed(t *testing.T) {
	group := cloudwatch.Group{Account: "123456789012", Region: "us-east-1", LogGroup: "/aws/ecs/paymentservice"}
	streams := []string{"ecs/paymentservice/0e1f", "ecs/paymentservice/2a3b"}

	var rendered []string
	for _, name := range streams {
		rendered = append(rendered, hex.EncodeToString(cwcheckpoint.Key(streamIn(group, name))))
	}

	golden.String(t, "checkpoint_keys.hex", strings.Join(rendered, "\n")+"\n")
}
