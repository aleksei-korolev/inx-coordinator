package coordinator

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/iotaledger/hive.go/core/syncutils"
	"github.com/iotaledger/hornet/v2/pkg/common"
	iotago "github.com/iotaledger/iota.go/v3"
	"github.com/iotaledger/iota.go/v3/nodeclient"
)

var (
	// ErrQuorumMerkleTreeHashMismatch is fired when a client in the quorum returns a different merkle tree hash.
	ErrQuorumMerkleTreeHashMismatch = errors.New("coordinator quorum merkle tree hash mismatch")
	// ErrQuorumGroupNoAnswer is fired when none of the clients in a quorum group answers.
	ErrQuorumGroupNoAnswer = errors.New("coordinator quorum group did not answer in time")
)

// QuorumClientConfig holds the configuration of a quorum client.
type QuorumClientConfig struct {
	// optional alias of the quorum client.
	Alias string `json:"alias" koanf:"alias"`
	// baseURL of the quorum client.
	BaseURL string `json:"baseUrl" koanf:"baseUrl"`
	// optional username for basic auth.
	Username string `json:"username" koanf:"username"`
	// optional password for basic auth.
	Password string `json:"password" koanf:"password"`
}

// QuorumClientStatistic holds statistics of a quorum client.
type QuorumClientStatistic struct {
	// name of the quorum group the client is member of.
	Group string
	// optional alias of the quorum client.
	Alias string
	// baseURL of the quorum client.
	BaseURL string
	// last response time of the whiteflag API call.
	ResponseTimeSeconds float64
	// error of last whiteflag API call.
	Error error
}

// QuorumFinishedResult holds statistics of a finished quorum.
type QuorumFinishedResult struct {
	Duration time.Duration
	Err      error
}

// quorumGroupEntry holds the api and statistics of a quorum client.
type quorumGroupEntry struct {
	api   *nodeclient.Client
	stats *QuorumClientStatistic
}

// quorum is used to check the correct ledger state of the coordinator.
type quorum struct {
	// the different groups of the quorum.
	Groups map[string][]*quorumGroupEntry
	// the maximim timeout of a quorum request.
	Timeout time.Duration

	quorumStatsLock syncutils.RWMutex
}

// newQuorum creates a new quorum, which is used to check the correct ledger state of the coordinator.
// If no groups are given, nil is returned.
func newQuorum(quorumGroups map[string][]*QuorumClientConfig, timeout time.Duration) *quorum {
	if len(quorumGroups) == 0 {
		panic("coordinator quorum groups not found")
	}

	groups := make(map[string][]*quorumGroupEntry)
	for groupName, groupNodes := range quorumGroups {
		if len(groupNodes) == 0 {
			panic(fmt.Sprintf("invalid coo quorum group: %s, no nodes given", groupName))
		}

		groups[groupName] = make([]*quorumGroupEntry, len(groupNodes))
		for i, client := range groupNodes {
			var userInfo *url.Userinfo
			if client.Username != "" || client.Password != "" {
				userInfo = url.UserPassword(client.Username, client.Password)
			}

			groups[groupName][i] = &quorumGroupEntry{
				api: nodeclient.New(client.BaseURL,
					nodeclient.WithHTTPClient(&http.Client{Timeout: timeout}),
					nodeclient.WithUserInfo(userInfo),
				),
				stats: &QuorumClientStatistic{
					Group:   groupName,
					Alias:   client.Alias,
					BaseURL: client.BaseURL,
				},
			}
		}
	}

	return &quorum{
		Groups:  groups,
		Timeout: timeout,
	}
}

// checkMerkleTreeHashQuorumGroup asks all nodes in a quorum group for their merkle tree hash based on the given parents.
// Returns non-critical and critical errors.
// If no node of the group answers, a non-critical error is returned.
// If one of the nodes returns a different hash, a critical error is returned.
func (q *quorum) checkMerkleTreeHashQuorumGroup(cooMerkleProof *MilestoneMerkleRoots,
	groupName string,
	quorumGroupEntries []*quorumGroupEntry,
	wg *sync.WaitGroup,
	quorumDoneChan chan struct{},
	quorumErrChan chan error,
	index iotago.MilestoneIndex,
	timestamp uint32,
	parents iotago.BlockIDs,
	previousMilestoneID iotago.MilestoneID,
	onGroupEntryError func(groupName string, entry *quorumGroupEntry, err error)) {
	// mark the group as done at the end
	defer wg.Done()

	// cancel the quorum after a certain timeout
	ctx, cancel := context.WithTimeout(context.Background(), q.Timeout)
	defer cancel()

	// create buffered channels, so the go routines will not be dangling if no receiver waits for the results anymore
	// garbage collector will take care if the channels are not used anymore. no need to close manually
	nodeResultChan := make(chan *nodeclient.ComputeWhiteFlagMutationsResponse, len(quorumGroupEntries))
	nodeErrorChan := make(chan error, len(quorumGroupEntries))

	for _, entry := range quorumGroupEntries {
		go func(entry *quorumGroupEntry, nodeResultChan chan *nodeclient.ComputeWhiteFlagMutationsResponse, nodeErrorChan chan error) {
			ts := time.Now()

			response, err := entry.api.ComputeWhiteFlagMutations(ctx, index, timestamp, parents, previousMilestoneID)

			// set the stats for the node
			entry.stats.ResponseTimeSeconds = time.Since(ts).Seconds()
			entry.stats.Error = err

			if err != nil {
				if onGroupEntryError != nil {
					onGroupEntryError(groupName, entry, err)
				}
				nodeErrorChan <- err

				return
			}
			nodeResultChan <- response
		}(entry, nodeResultChan, nodeErrorChan)
	}

	//nolint:ifshort // false positive
	validResults := 0
QuorumLoop:
	for i := 0; i < len(quorumGroupEntries); i++ {
		// we wait either until the channel got closed or the context is done
		select {
		case <-quorumDoneChan:
			// quorum was aborted
			return

		case <-nodeErrorChan:
			// ignore errors of single nodes
			continue

		case nodeWhiteFlagResponse := <-nodeResultChan:
			if cooMerkleProof.AppliedMerkleRoot != nodeWhiteFlagResponse.AppliedMerkleRoot ||
				cooMerkleProof.InclusionMerkleRoot != nodeWhiteFlagResponse.InclusionMerkleRoot {
				// mismatch of the merkle tree hash of the node => critical error
				quorumErrChan <- common.CriticalError(ErrQuorumMerkleTreeHashMismatch)

				return
			}
			validResults++

		case <-ctx.Done():
			// quorum timeout reached
			break QuorumLoop
		}
	}

	if validResults == 0 {
		// no node of the group answered, return a non-critical error.
		quorumErrChan <- common.SoftError(ErrQuorumGroupNoAnswer)
	}
}

// checkMerkleTreeHash asks all nodes in the quorum for their merkle tree hash based on the given parents.
// Returns non-critical and critical errors.
// If no node of a certain group answers, a non-critical error is returned.
// If one of the nodes returns a different hash, a critical error is returned.
func (q *quorum) checkMerkleTreeHash(cooMerkleProof *MilestoneMerkleRoots,
	index iotago.MilestoneIndex,
	timestamp uint32,
	parents iotago.BlockIDs,
	previousMilestoneID iotago.MilestoneID,
	onGroupEntryError func(groupName string, entry *quorumGroupEntry, err error)) error {
	q.quorumStatsLock.Lock()
	defer q.quorumStatsLock.Unlock()

	wg := &sync.WaitGroup{}
	quorumDoneChan := make(chan struct{})
	quorumErrChan := make(chan error)

	for groupName, quorumGroupEntries := range q.Groups {
		wg.Add(1)

		// ask all groups in parallel
		go q.checkMerkleTreeHashQuorumGroup(cooMerkleProof, groupName, quorumGroupEntries, wg, quorumDoneChan, quorumErrChan, index, timestamp, parents, previousMilestoneID, onGroupEntryError)
	}

	go func(wg *sync.WaitGroup, doneChan chan struct{}) {
		// wait for all groups to be finished
		wg.Wait()

		// signal that all groups are finished
		close(doneChan)
	}(wg, quorumDoneChan)

	select {
	case <-quorumDoneChan:
		// quorum finished successfully
		return nil

	case err := <-quorumErrChan:
		// quorum encountered an error
		return err
	}
}

// quorumStatsSnapshot returns a snapshot of the statistics about the response time and errors of every node in the quorum.
func (q *quorum) quorumStatsSnapshot() []QuorumClientStatistic {
	q.quorumStatsLock.RLock()
	defer q.quorumStatsLock.RUnlock()

	var stats []QuorumClientStatistic

	for _, quorumGroup := range q.Groups {
		for _, entry := range quorumGroup {
			stats = append(stats, *entry.stats)
		}
	}

	return stats
}
