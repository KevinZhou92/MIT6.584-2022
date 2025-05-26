package raft

import (
	"fmt"
	"math/rand"
	"time"
)

// example RequestVote RPC arguments structure.
// field names must start with capital letters!
type RequestVoteArgs struct {
	// Your data here (3A, 3B).
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

func (args RequestVoteArgs) String() string {
	return fmt.Sprintf("RequestVoteArgs{Term: %d, CandidateId: %d, LastLogIndex: %d, LastLogTerm: %d}",
		args.Term, args.CandidateId, args.LastLogIndex, args.LastLogTerm)
}

// example RequestVote RPC reply structure.
// field names must start with capital letters!
type RequestVoteReply struct {
	// Your data here (3A).
	Term        int
	VoteGranted bool
}

type ElectionState struct {
	Role        Role
	CurrentTerm int
	VotedFor    int // index of the candidate that this peer voted for
}

func (state ElectionState) String() string {
	return fmt.Sprintf("ElectionState: role: %s, CurrentTerm: %d, VotedFor: %d", roleMap[state.Role], state.CurrentTerm, state.VotedFor)
}

func (rf *Raft) shoudStartElection(timeout time.Duration) bool {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	curTime := time.Now()

	return rf.lastCommTime.Add(timeout).Before(curTime) && (rf.electionState.Role == FOLLOWER || rf.electionState.Role == CANDIDATE)
}

func (rf *Raft) election() {
	currentTerm := rf.getCurrentTerm()
	Debug(dVote, "Server %d started election, current term %d, current role %s", rf.me, currentTerm, roleMap[rf.GetRole()])
	ms := 300 + (rand.Int63() % 200)
	deadline := time.After(time.Duration(ms) * time.Millisecond)

	resultCh := make(chan RequestVoteReply, len(rf.peers))
	for idx := range rf.peers {
		if idx == rf.me {
			continue
		}
		if !rf.isCandidate() {
			return
		}

		go rf.getVote(resultCh, idx)
	}

	votes := 1
	collected := 1
	for collected < len(rf.peers) {
		select {
		case reply := <-resultCh:
			if !rf.isCandidate() {
				Debug(dVote, "Server %d received a vote, however server's current role is %s\n", rf.me, roleMap[rf.GetRole()])
				return
			}
			if reply.VoteGranted {
				votes += 1
				Debug(dVote, "Server %d received a vote, current vote %d\n", rf.me, votes)
			}
			// Found a peer with a greater term, this peer can't become the leader anymore, stop the leader election
			if reply.Term > currentTerm {
				Debug(dVote, "Server %d's term %d is lower than peer's term %d", rf.me, currentTerm, reply.Term)
				rf.setState(FOLLOWER, reply.Term, -1)
				return
			}
			collected += 1
			// Elected as leader
			if votes > len(rf.peers)/2 {
				Debug(dLeader, "Server %d has %d votes with term %d(log size: %d) and is LEADER now!\n", rf.me, votes, currentTerm, rf.getLogSizeWithSnapshotInfo())
				rf.setState(LEADER, currentTerm, rf.me)
				rf.initializePeerIndexState()
				rf.startLeaderProcesses()

				return
			}
		case <-deadline:
			Debug(dInfo, "Server %d nothing happened during election for term %d. Waiting for new election\n", rf.me, currentTerm)
			return
		}
	}
}

func (rf *Raft) startLeaderProcesses() {
	// rf.appendLogLocally(LogEntry{rf.getCurrentTerm(), -1})
	for server := range rf.peers {
		if server == rf.me {
			continue
		}
		// append a no-op entry at the beginnin of leader term, to make sure we can rely on
		// this message to commit entry in a reconnected server, so that all servers can catch up
		// Basically, this is a fake client write to froce all the peers to get to an agreement
		go rf.sendAppendEntries(server, rf.buildHeartBeatArgs(), &AppendEntriesReply{})
		go rf.runLogReplicator(server)
	}

	go rf.runReplicaCounter()
	go rf.runHeartbeatProcess()
}

// example RequestVote RPC handler.
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	// Your code here (3A, 3B).
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// we should not update lastCommtime if someone is requesting for vote, otherwise we could let the unqualified
	// peer keep requesting for vote
	// rf.lastCommTime = time.Now()

	Debug(dVote, "Server %d received request vote %v, current electionState: %s\n", rf.me, args, rf.electionState)
	requestTerm, candidateId, lastLogIndex, lastLogTerm := args.Term, args.CandidateId, args.LastLogIndex, args.LastLogTerm
	reply.VoteGranted = false
	// requester's term is smaller than current peer's term
	if requestTerm < rf.electionState.CurrentTerm {
		reply.Term = rf.electionState.CurrentTerm
		return
	}

	// requester's term is greater than current peer's term, convert current peer to follower
	if requestTerm > rf.electionState.CurrentTerm {
		Debug(dVote, "Server %d's term %d is lower than server %d's term %d\n", rf.me, rf.electionState.CurrentTerm, candidateId, requestTerm)
		rf.electionState.CurrentTerm = requestTerm
		rf.electionState.Role = FOLLOWER
		rf.electionState.VotedFor = -1
		rf.persist()
	}

	// if lastLogTerm < rf.logs[len(rf.logs)-1].Term || (lastLogTerm == rf.logs[len(rf.logs)-1].Term && lastLogIndex < absoluteIndex) {
	if !rf.isCandidateLogUpToDate(lastLogIndex, lastLogTerm, candidateId) {
		reply.Term = rf.electionState.CurrentTerm
		reply.VoteGranted = false
		return
	}

	// vote for requester
	if rf.electionState.VotedFor == -1 || rf.electionState.VotedFor == candidateId {
		// Reset last comm time if current peer just voted for a candiat
		rf.lastCommTime = time.Now()
		rf.electionState.VotedFor = candidateId
		rf.electionState.CurrentTerm = requestTerm
		reply.VoteGranted = true
		reply.Term = requestTerm
		rf.persist()
		Debug(dVote, "Server %d votedFor server %d, electionState: %v\n", rf.me, candidateId, rf.electionState)
		return
	}
}

// Check if requester's log is more up-to-date
func (rf *Raft) isCandidateLogUpToDate(peerLastLogIndex int, peerLastLogTerm int, server int) bool {
	var lastLogTerm int
	lastLogIndex := rf.snapshotState.LastIncludedIndex + len(rf.logs)

	if len(rf.logs) == 0 {
		lastLogTerm = rf.snapshotState.LastIncludedTerm
	} else {
		lastLogTerm = rf.logs[len(rf.logs)-1].Term
	}

	Debug(dVote, "Server %d's lastLogIndex %d and lastLogTerm %d and candidate %d's lastLogIndex %d and lastLogTerm %d\n",
		rf.me, lastLogIndex, lastLogTerm, server, peerLastLogIndex, peerLastLogTerm)
	if lastLogTerm != peerLastLogTerm {
		return peerLastLogTerm >= lastLogTerm
	}

	return peerLastLogIndex >= lastLogIndex
}

func (rf *Raft) getVote(resultChan chan RequestVoteReply, server int) {
	lastLogIndex := rf.getLogSizeWithSnapshotInfo() - 1
	lastLogEntry, err := rf.getLogEntryWithSnapshotInfo(lastLogIndex)
	if err != nil {
		Debug(dError, "Server %d can't get log from lastLogIndex %d, log size: %d, give up get Vote", rf.me, lastLogIndex, rf.getLogSizeWithSnapshotInfo())
		return
	}
	lastLogTerm := lastLogEntry.Term
	currentTerm := rf.getCurrentTerm()

	requestVoteArgs := RequestVoteArgs{currentTerm, rf.me, lastLogIndex, lastLogTerm}
	requestVoteReply := RequestVoteReply{}
	ok := rf.sendRequestVote(server, &requestVoteArgs, &requestVoteReply)
	if !ok {
		return
	}

	resultChan <- requestVoteReply
}

// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}
