package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	//	"bytes"

	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	//	"6.5840/labgob"

	"6.5840/labrpc"
)

// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in part 3D you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh, but set CommandValid to false for these
// other uses.
type Role int

const (
	LEADER Role = iota
	CANDIDATE
	FOLLOWER
)

type SnapshotState struct {
	LastIncludedIndex int // index of last included entry in snapshot, this information will be used to get entry from truncated rf.logs
	LastIncludedTerm  int // term of laster included entry in snapshot
}

func (state *SnapshotState) String() string {
	return fmt.Sprintf("SnapshotState: LastIncludedIndex: %d, LastIncludedTerm: %d", state.LastIncludedIndex, state.LastIncludedTerm)
}

// A go object recording the index state for the logs
type LogState struct {
	commitIndex      int
	lastAppliedIndex int
}

func (logState *LogState) String() string {
	return fmt.Sprintf("LogState: commitIndex: %d, lastAppliedIndex: %d", logState.commitIndex, logState.lastAppliedIndex)
}

// A go object recording the index state for each peer
type PeerIndexState struct {
	nextIndex  []int // index of next log entry to send to ith server
	matchIndex []int // index of highest log entry known to be replicated on server ith
}

// A Go object implementing a single Raft peer.
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	// Your data here (3A, 3B, 3C).
	// Look at the paper's Figure 2 for a description of what
	// serverState a Raft server must maintain.
	electionState *ElectionState // state of current server, term and is role
	lastCommTime  time.Time

	logs           []LogEntry
	logState       *LogState       // the commited log index and the applied log index, here applied means the result has been returned to client
	peerIndexState *PeerIndexState // the next index and match index for each peer

	applyCond *sync.Cond
	applyCh   chan ApplyMsg

	// Snapshot section
	snapshotState           *SnapshotState
	pendingSnapshotApplyMsg *ApplyMsg
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	var term int
	var isleader bool
	// Your code here (3A).
	term = rf.electionState.CurrentTerm
	isleader = rf.electionState.Role == LEADER
	Debug(dLog, "Server %d(term: %d) is %s", rf.me, term, roleMap[rf.electionState.Role])

	return term, isleader
}

// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	// We need to check if it is leader and then append log
	// if we retrieve the term first and then check if current peer is a leader
	// we could retrieve an old term
	rf.mu.Lock()
	defer rf.mu.Unlock()

	term, isLeader := rf.electionState.CurrentTerm, rf.electionState.Role == LEADER

	Debug(dLog, "Server %d(term: %d) is %s and Start() received command %v", rf.me, term, roleMap[rf.electionState.Role], command)

	// return immediately if current peer is not leader
	if !isLeader {
		return -1, -1, false
	}

	index := rf.appendLogLocally(LogEntry{term, command})

	return index, term, isLeader
}

// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

func (rf *Raft) ticker() {
	for !rf.killed() {
		// Your code here (3A)
		// Check if a leader election should be started.
		// pause for a random amount of time between 50 and 350
		// milliseconds.
		timeout := (300 + time.Duration(rand.Int63()%200)) * time.Millisecond
		time.Sleep(timeout)
		// Debug(dLog, "Server %d is %s", rf.me, roleMap[rf.state.role])
		// Debug(dTerm, "Server %d term is %d", rf.me, rf.state.currentTerm)
		if rf.shoudStartElection(timeout) {
			// increment term and start a new election
			rf.setState(CANDIDATE, rf.getCurrentTerm()+1, rf.me)
			rf.election()
		}
	}
}

// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	// Your initialization code here (3A, 3B, 3C).
	rf.lastCommTime = time.Now()
	rf.electionState = &ElectionState{FOLLOWER, 0, -1}
	rf.logs = []LogEntry{
		{Term: 0, Command: 0},
	}
	// rf.logState = &LogState{len(rf.logs) - 1, 0}
	rf.applyCh = applyCh

	rf.snapshotState = &SnapshotState{-1, -1}
	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())
	// A peer would always starts as a follower
	rf.setState(FOLLOWER, rf.getCurrentTerm(), -1)

	rf.logState = &LogState{max(rf.snapshotState.LastIncludedIndex, 0), max(rf.snapshotState.LastIncludedIndex, 0)}
	Debug(dInfo, "Server %d started with electionState: %v, logs: %v, snapshotState: %v", rf.me, rf.electionState, rf.logs, rf.snapshotState)

	rf.initializePeerIndexState()
	Debug(dInfo, "Server %d started with PeerIndexState: %v", rf.me, rf.peerIndexState)

	rf.applyCond = sync.NewCond(&rf.mu)

	// start ticker goroutine to start elections
	go rf.ticker()

	// start log replicator if raft peer is a leader
	if rf.isLeader() {
		rf.startLeaderProcesses()
	}

	// Apply committed message on each peer
	go rf.runApplier()

	Debug(dInfo, "Server %d started with term %d", rf.me, rf.getCurrentTerm())

	return rf
}
