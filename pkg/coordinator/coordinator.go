package coordinator

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/pkg/errors"

	"github.com/iotaledger/hive.go/core/events"
	"github.com/iotaledger/hive.go/core/ioutils"
	"github.com/iotaledger/hive.go/core/logger"
	"github.com/iotaledger/hive.go/core/syncutils"
	"github.com/iotaledger/hornet/v2/pkg/common"
	"github.com/iotaledger/inx-coordinator/pkg/migrator"
	iotago "github.com/iotaledger/iota.go/v3"

	// import implementation.
	_ "golang.org/x/crypto/blake2b"
)

// BackPressureFunc is a function which tells the Coordinator
// to stop issuing milestones and checkpoints under high load.
type BackPressureFunc func() bool

// SendBlockFunc is a function which sends a block to the network.
type SendBlockFunc = func(block *iotago.Block, msIndex ...iotago.MilestoneIndex) (iotago.BlockID, error)

// LatestMilestoneInfo contains the info of the latest milestone the connected node knows.
type LatestMilestoneInfo struct {
	Index       iotago.MilestoneIndex
	Timestamp   uint32
	MilestoneID iotago.MilestoneID
}

// LatestTreasuryOutput represents the latest treasury output created by the last milestone that contained a migration.
type LatestTreasuryOutput struct {
	MilestoneID iotago.MilestoneID
	Amount      uint64
}

// UnspentTreasuryOutputFunc should return the latest unspent LatestTreasuryOutput.
type UnspentTreasuryOutputFunc = func() (*LatestTreasuryOutput, error)

var (
	// ErrNoTipsGiven is returned when no tips were given to issue a checkpoint.
	ErrNoTipsGiven = errors.New("no tips given")
	// ErrNetworkBootstrapped is returned when the flag for bootstrap network was given, but a state file already exists.
	ErrNetworkBootstrapped = errors.New("network already bootstrapped")
	// ErrNodeLoadTooHigh is returned if the backpressure func says the node load is too high.
	ErrNodeLoadTooHigh = errors.New("node load too high")
)

// Events are the events issued by the coordinator.
type Events struct {
	// Fired when a checkpoint block is issued.
	IssuedCheckpointBlock *events.Event
	// Fired when a milestone is issued.
	IssuedMilestone *events.Event
	// SoftError is triggered when a soft error is encountered.
	SoftError *events.Event
	// QuorumFinished is triggered after a coordinator quorum call was finished.
	QuorumFinished *events.Event
}

// IsNodeSyncedFunc should only return true if the node connected to the coordinator is synced.
type IsNodeSyncedFunc = func() bool

// ProtocolParameteresFunc should return the current valid protocol parameters.
type ProtocolParameteresFunc = func() *iotago.ProtocolParameters

// MilestoneMerkleRoots contains the merkle roots calculated by whiteflag confirmation.
type MilestoneMerkleRoots struct {
	// InclusionMerkleRoot is the root of the merkle tree containing the hash of all included blocks.
	InclusionMerkleRoot iotago.MilestoneMerkleProof
	// AppliedMerkleRoot is the root of the merkle tree containing the hash of all include blocks that mutate the ledger.
	AppliedMerkleRoot iotago.MilestoneMerkleProof
}

type ComputeMilestoneMerkleRoots = func(ctx context.Context, index iotago.MilestoneIndex, timestamp uint32, parents iotago.BlockIDs, previousMilestoneID iotago.MilestoneID) (*MilestoneMerkleRoots, error)

// Coordinator is used to issue signed blocks, called "milestones" to secure an IOTA network and prevent double spends.
type Coordinator struct {
	// the logger used to log events.
	*logger.WrappedLogger
	// used to compute the merkle roots used inside the milestone payload.
	merkleRootFunc ComputeMilestoneMerkleRoots
	// used to issue only one milestone at a time.
	milestoneLock syncutils.Mutex
	// used to determine the sync status of the node.
	isNodeSynced IsNodeSyncedFunc
	// Used to determine the current protocol parameters including byte costs.
	protoParamsFunc ProtocolParameteresFunc
	// used to get receipts for the WOTS migration.
	migratorService *migrator.Service
	// used to get the treasury output.
	treasuryOutputFunc UnspentTreasuryOutputFunc
	// used to sign the milestones.
	signerProvider MilestoneSignerProvider
	// the function used to send a block.
	sendBlockFunc SendBlockFunc
	// holds the coordinator options.
	opts *Options

	// back pressure functions that signal congestion.
	backpressureFuncs []BackPressureFunc
	// state of the coordinator holds information about the last issued milestones.
	state *State
	// whether the coordinator was bootstrapped.
	bootstrapped bool
	// events of the coordinator.
	Events *Events
}

const (
	defaultStateFilePath     = "coordinator.state"
	defaultMilestoneInterval = time.Duration(10) * time.Second
)

var (
	emptyMilestoneID = iotago.MilestoneID{}
)

// the default options applied to the Coordinator.
var defaultOptions = []Option{
	WithStateFilePath(defaultStateFilePath),
	WithMilestoneInterval(defaultMilestoneInterval),
	WithSigningRetryAmount(10),
	WithSigningRetryTimeout(2 * time.Second),
}

// Options define options for the Coordinator.
type Options struct {
	// the logger used to log events.
	logger *logger.Logger
	// the path to the state file of the coordinator.
	stateFilePath string
	// the interval milestones are issued.
	milestoneInterval time.Duration
	// the timeout between signing retries.
	signingRetryTimeout time.Duration
	// the amount of times to retry signing before bailing and shutting down the Coordinator.
	signingRetryAmount int
	// the optional quorum used by the coordinator to check for correct ledger state calculation.
	quorum *quorum
}

// applies the given Option.
func (so *Options) apply(opts ...Option) {
	for _, opt := range opts {
		opt(so)
	}
}

// WithLogger enables logging within the coordinator.
func WithLogger(logger *logger.Logger) Option {
	return func(opts *Options) {
		opts.logger = logger
	}
}

// WithStateFilePath defines the path to the state file of the coordinator.
func WithStateFilePath(stateFilePath string) Option {
	return func(opts *Options) {
		opts.stateFilePath = stateFilePath
	}
}

// WithMilestoneInterval defines interval milestones are issued.
func WithMilestoneInterval(milestoneInterval time.Duration) Option {
	return func(opts *Options) {
		opts.milestoneInterval = milestoneInterval
	}
}

// WithSigningRetryTimeout defines signing retry timeout.
func WithSigningRetryTimeout(timeout time.Duration) Option {
	return func(opts *Options) {
		opts.signingRetryTimeout = timeout
	}
}

// WithSigningRetryAmount defines signing retry amount.
func WithSigningRetryAmount(amount int) Option {
	return func(opts *Options) {
		opts.signingRetryAmount = amount
	}
}

// WithQuorum defines a quorum, which is used to check the correct ledger state of the coordinator.
// If no quorumGroups are given, the quorum is disabled.
func WithQuorum(quorumEnabled bool, quorumGroups map[string][]*QuorumClientConfig, timeout time.Duration) Option {
	return func(opts *Options) {
		if !quorumEnabled {
			opts.quorum = nil

			return
		}
		opts.quorum = newQuorum(quorumGroups, timeout)
	}
}

// Option is a function setting a coordinator option.
type Option func(opts *Options)

// New creates a new coordinator instance.
func New(
	merkleRootFunc ComputeMilestoneMerkleRoots,
	nodeSyncedFunc IsNodeSyncedFunc,
	protoParamsFunc ProtocolParameteresFunc,
	signerProvider MilestoneSignerProvider,
	migratorService *migrator.Service,
	treasuryOutputFunc UnspentTreasuryOutputFunc,
	sendBlockFunc SendBlockFunc,
	opts ...Option) (*Coordinator, error) {

	options := &Options{}
	options.apply(defaultOptions...)
	options.apply(opts...)

	if migratorService != nil && treasuryOutputFunc == nil {
		return nil, common.CriticalError(errors.New("migrator configured, but no treasury output fetch function provided"))
	}

	result := &Coordinator{
		merkleRootFunc:     merkleRootFunc,
		isNodeSynced:       nodeSyncedFunc,
		protoParamsFunc:    protoParamsFunc,
		signerProvider:     signerProvider,
		migratorService:    migratorService,
		treasuryOutputFunc: treasuryOutputFunc,
		sendBlockFunc:      sendBlockFunc,
		opts:               options,

		Events: &Events{
			IssuedCheckpointBlock: events.NewEvent(CheckpointCaller),
			IssuedMilestone:       events.NewEvent(MilestoneCaller),
			SoftError:             events.NewEvent(events.ErrorCaller),
			QuorumFinished:        events.NewEvent(QuorumFinishedCaller),
		},
	}
	result.WrappedLogger = logger.NewWrappedLogger(options.logger)

	return result, nil
}

// InitState loads an existing state file or bootstraps the network.
// All errors are critical.
func (coo *Coordinator) InitState(bootstrap bool, startIndex iotago.MilestoneIndex, latestMilestone *LatestMilestoneInfo) error {

	_, err := os.Stat(coo.opts.stateFilePath)
	stateFileExists := !os.IsNotExist(err)

	if bootstrap {
		if stateFileExists {
			return ErrNetworkBootstrapped
		}

		if startIndex == 0 {
			// start with milestone 1 at least
			startIndex = 1
		}

		if latestMilestone.Index != startIndex-1 {
			return fmt.Errorf("previous milestone does not match latest milestone in node! previous: %d, INX: %d", startIndex-1, latestMilestone.Index)
		}

		latestMilestoneID := iotago.MilestoneID{}
		if startIndex != 1 {

			if latestMilestone.MilestoneID == emptyMilestoneID {
				return fmt.Errorf("previous milestone milestoneID should not be genesis")
			}

			// If we don't start a new network, the last milestone has to be referenced
			latestMilestoneID = latestMilestone.MilestoneID
		}

		// create a new coordinator state to bootstrap the network
		state := &State{}
		state.LatestMilestoneBlockID = iotago.EmptyBlockID()
		state.LatestMilestoneID = latestMilestoneID
		state.LatestMilestoneIndex = startIndex - 1
		state.LatestMilestoneTime = time.Now()

		coo.state = state
		coo.bootstrapped = false

		coo.LogInfof("bootstrapping coordinator at %d", startIndex)

		return nil
	}

	if !stateFileExists {
		return fmt.Errorf("state file not found: %v", coo.opts.stateFilePath)
	}

	coo.state = &State{}
	if err := ioutils.ReadJSONFromFile(coo.opts.stateFilePath, coo.state); err != nil {
		return err
	}

	if latestMilestone.Index != coo.state.LatestMilestoneIndex {
		return fmt.Errorf("previous milestone does not match latest milestone in node. previous: %d, INX: %d", coo.state.LatestMilestoneIndex, latestMilestone.Index)
	}

	coo.LogInfof("resuming coordinator at %d", latestMilestone.Index)

	coo.bootstrapped = true

	return nil
}

// createAndSendMilestone creates a milestone, sends it to the network and stores a new coordinator state file.
// Returns non-critical and critical errors.
func (coo *Coordinator) createAndSendMilestone(parents iotago.BlockIDs, newMilestoneIndex iotago.MilestoneIndex, previousMilestoneID iotago.MilestoneID) error {

	parents = parents.RemoveDupsAndSort()

	// We have to set a timestamp for when we run the white-flag mutations due to the semantic validation.
	// This should be exactly the same one used when issuing the milestone later on.
	newMilestoneTimestamp := time.Now()

	// compute merkle tree root
	// we pass a background context here to not cancel the white-flag computation!
	// otherwise the coordinator could panic at shutdown.
	merkleProof, err := coo.merkleRootFunc(context.Background(), newMilestoneIndex, uint32(newMilestoneTimestamp.Unix()), parents, previousMilestoneID)
	if err != nil {
		return common.CriticalError(fmt.Errorf("failed to compute white flag mutations: %w", err))
	}

	// ask the quorum for correct ledger state if enabled
	if coo.opts.quorum != nil {
		ts := time.Now()
		err := coo.opts.quorum.checkMerkleTreeHash(merkleProof, newMilestoneIndex, uint32(newMilestoneTimestamp.Unix()), parents, previousMilestoneID, func(groupName string, entry *quorumGroupEntry, err error) {
			coo.LogInfof("coordinator quorum group encountered an error, group: %s, baseURL: %s, err: %s", groupName, entry.stats.BaseURL, err)
		})

		duration := time.Since(ts)
		coo.Events.QuorumFinished.Trigger(&QuorumFinishedResult{Duration: duration, Err: err})

		if err != nil {
			// quorum failed => non-critical or critical error
			coo.LogInfof("coordinator quorum failed after %v, err: %s", time.Since(ts).Truncate(time.Millisecond), err)

			return err
		}

		coo.LogInfof("coordinator quorum took %v", duration.Truncate(time.Millisecond))
	}

	// get receipt data in case migrator is enabled
	var receipt *iotago.ReceiptMilestoneOpt
	if coo.migratorService != nil {
		receipt = coo.migratorService.Receipt()
		if receipt != nil {
			if err := coo.migratorService.PersistState(true); err != nil {
				return common.CriticalError(fmt.Errorf("unable to persist migrator state before send: %w", err))
			}

			currentTreasuryOutput, err := coo.treasuryOutputFunc()
			if err != nil {
				return common.CriticalError(fmt.Errorf("unable to fetch unspent treasury output: %w", err))
			}

			// embed treasury within the receipt
			input := &iotago.TreasuryInput{}
			copy(input[:], currentTreasuryOutput.MilestoneID[:])
			output := &iotago.TreasuryOutput{Amount: currentTreasuryOutput.Amount - receipt.Sum()}
			treasuryTx := &iotago.TreasuryTransaction{Input: input, Output: output}
			receipt.Transaction = treasuryTx
			receipt.SortFunds()
		}
	}

	milestoneBlock, err := coo.createMilestone(newMilestoneIndex, uint32(newMilestoneTimestamp.Unix()), parents, receipt, previousMilestoneID, merkleProof)
	if err != nil {
		return common.CriticalError(fmt.Errorf("failed to create milestone: %w", err))
	}

	milestoneID, err := milestoneBlock.Payload.(*iotago.Milestone).ID()
	if err != nil {
		return common.CriticalError(fmt.Errorf("failed to compute milestone ID: %w", err))
	}

	// rename the coordinator state file to mark the state as invalid
	if err := os.Rename(coo.opts.stateFilePath, fmt.Sprintf("%s_old", coo.opts.stateFilePath)); err != nil && !os.IsNotExist(err) {
		return common.CriticalError(fmt.Errorf("unable to rename old coordinator state file: %w", err))
	}

	latestMilestoneBlockID, err := coo.sendBlockFunc(milestoneBlock, newMilestoneIndex)
	if err != nil {
		return common.CriticalError(fmt.Errorf("failed to send milestone: %w", err))
	}

	if coo.migratorService != nil && receipt != nil {
		if err := coo.migratorService.PersistState(false); err != nil {
			return common.CriticalError(fmt.Errorf("unable to persist migrator state after send: %w", err))
		}
	}

	// always reference the last milestone directly to speed up syncing
	coo.state.LatestMilestoneBlockID = latestMilestoneBlockID
	coo.state.LatestMilestoneID = milestoneID
	coo.state.LatestMilestoneIndex = newMilestoneIndex
	coo.state.LatestMilestoneTime = newMilestoneTimestamp

	if err := ioutils.WriteJSONToFile(coo.opts.stateFilePath, coo.state, 0660); err != nil {
		return common.CriticalError(fmt.Errorf("failed to update coordinator state file: %w", err))
	}

	coo.Events.IssuedMilestone.Trigger(coo.state.LatestMilestoneIndex, coo.state.LatestMilestoneID, coo.state.LatestMilestoneBlockID)

	return nil
}

// Bootstrap creates the first milestone, if the network was not bootstrapped yet.
// Returns critical errors.
func (coo *Coordinator) Bootstrap() (iotago.BlockID, error) {

	coo.milestoneLock.Lock()
	defer coo.milestoneLock.Unlock()

	if !coo.bootstrapped {
		// create first milestone to bootstrap the network
		// only one parent references the last known milestone or NullBlockID if startIndex = 1 (see InitState)
		err := coo.createAndSendMilestone(iotago.BlockIDs{coo.state.LatestMilestoneBlockID}, coo.state.LatestMilestoneIndex+1, coo.state.LatestMilestoneID)
		if err != nil {
			// creating milestone failed => always a critical error at bootstrap
			return iotago.EmptyBlockID(), common.CriticalError(err)
		}

		coo.bootstrapped = true
	}

	return coo.state.LatestMilestoneBlockID, nil
}

// IssueCheckpoint tries to create and send a "checkpoint" to the network.
// a checkpoint can contain multiple chained blocks to reference big parts of the unreferenced cone.
// this is done to keep the confirmation rate as high as possible, even if there is an attack ongoing.
// new checkpoints always reference the last checkpoint or the last milestone if it is the first checkpoint after a new milestone.
func (coo *Coordinator) IssueCheckpoint(checkpointIndex int, lastCheckpointBlockID iotago.BlockID, tips iotago.BlockIDs) (iotago.BlockID, error) {

	if len(tips) == 0 {
		return iotago.EmptyBlockID(), ErrNoTipsGiven
	}

	coo.milestoneLock.Lock()
	defer coo.milestoneLock.Unlock()

	if !coo.isNodeSynced() {
		return iotago.EmptyBlockID(), common.SoftError(common.ErrNodeNotSynced)
	}

	// check whether we should hold issuing checkpoints
	// if the node is currently under a lot of load
	if coo.checkBackPressureFunctions() {
		return iotago.EmptyBlockID(), common.SoftError(ErrNodeLoadTooHigh)
	}

	// maximum 8 parents per block (7 tips + last checkpoint blockID)
	checkpointsNumber := int(math.Ceil(float64(len(tips)) / 7.0))

	// issue several checkpoints until all tips are used
	for i := 0; i < checkpointsNumber; i++ {
		tipStart := i * 7
		tipEnd := tipStart + 7

		if tipEnd > len(tips) {
			tipEnd = len(tips)
		}

		parents := iotago.BlockIDs{lastCheckpointBlockID}
		parents = append(parents, tips[tipStart:tipEnd]...)
		parents = parents.RemoveDupsAndSort()

		block, err := coo.createCheckpoint(parents)
		if err != nil {
			return iotago.EmptyBlockID(), common.SoftError(fmt.Errorf("failed to create checkPoint: %w", err))
		}

		blockID, err := coo.sendBlockFunc(block)
		if err != nil {
			return iotago.EmptyBlockID(), common.SoftError(fmt.Errorf("failed to send checkPoint: %w", err))
		}

		lastCheckpointBlockID = blockID

		coo.Events.IssuedCheckpointBlock.Trigger(checkpointIndex, i, checkpointsNumber, lastCheckpointBlockID)
	}

	return lastCheckpointBlockID, nil
}

// IssueMilestone creates the next milestone.
// Returns non-critical and critical errors.
func (coo *Coordinator) IssueMilestone(parents iotago.BlockIDs) (iotago.BlockID, error) {

	coo.milestoneLock.Lock()
	defer coo.milestoneLock.Unlock()

	if !coo.isNodeSynced() {
		// return a non-critical error to not kill the database
		return iotago.EmptyBlockID(), common.SoftError(common.ErrNodeNotSynced)
	}

	// check whether we should hold issuing miletones
	// if the node is currently under a lot of load
	if coo.checkBackPressureFunctions() {
		return iotago.EmptyBlockID(), common.SoftError(ErrNodeLoadTooHigh)
	}

	if err := coo.createAndSendMilestone(parents, coo.state.LatestMilestoneIndex+1, coo.state.LatestMilestoneID); err != nil {
		// creating milestone failed => non-critical or critical error
		return iotago.EmptyBlockID(), err
	}

	return coo.state.LatestMilestoneBlockID, nil
}

// Interval returns the interval milestones should be issued.
func (coo *Coordinator) Interval() time.Duration {
	return coo.opts.milestoneInterval
}

// State returns the current state of the coordinator.
func (coo *Coordinator) State() *State {
	return coo.state
}

// AddBackPressureFunc adds a BackPressureFunc.
// This function can be called multiple times to add additional BackPressureFunc.
func (coo *Coordinator) AddBackPressureFunc(bpFunc BackPressureFunc) {
	coo.backpressureFuncs = append(coo.backpressureFuncs, bpFunc)
}

// checkBackPressureFunctions checks whether any back pressure function is signaling congestion.
func (coo *Coordinator) checkBackPressureFunctions() bool {
	for _, f := range coo.backpressureFuncs {
		if f() {
			return true
		}
	}

	return false
}

// QuorumStats returns statistics about the response time and errors of every node in the quorum.
func (coo *Coordinator) QuorumStats() []QuorumClientStatistic {
	if coo.opts.quorum == nil {
		return nil
	}

	return coo.opts.quorum.quorumStatsSnapshot()
}
