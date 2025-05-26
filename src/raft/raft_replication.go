package raft

import (
	"fmt"
	"time"
)

type LogEntry struct {
	Term    int
	Command interface{}
}

type AppendEntriesArgs struct {
	// Your data here (3A, 3B).
	Term         int
	LeaderId     int
	Entries      []LogEntry
	PrevLogIndex int
	PrevLogTerm  int
	LeaderCommit int
}

func (args AppendEntriesArgs) String() string {
	return fmt.Sprintf(
		"AppendEntriesArgs{Term: %d, LeaderId: %d, PrevLogIndex: %d, PrevLogTerm: %d, LeaderCommit: %d, Entries: %v}",
		args.Term, args.LeaderId, args.PrevLogIndex, args.PrevLogTerm, args.LeaderCommit, args.Entries,
	)
}

type AppendEntriesReply struct {
	// Your data here (3A, 3B).
	Term    int
	Success bool

	// Conflict Info
	ConflictEntryIndex int
	ConflictEntryTerm  int
	PeerLogSize        int
}

func (args AppendEntriesReply) String() string {
	return fmt.Sprintf(
		"AppendEntriesReply{Term: %d, Success: %t, ConflictEntryIndex: %d, ConflictEntryTerm: %d, PeerLogSize: %d}",
		args.Term, args.Success, args.ConflictEntryIndex, args.ConflictEntryTerm, args.PeerLogSize)
}

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	// For 3D:
	SnapshotValid bool
	Snapshot      []byte
	SnapshotTerm  int
	SnapshotIndex int
}

func (applyMsg ApplyMsg) String() string {
	return fmt.Sprintf("ApplyMsg: CommandValid: %t, Command: %d, CommandIndex: %d, SnapshotValid: %t, SnapshotTerm: %d, SnapshotIndex: %d",
		applyMsg.CommandValid, applyMsg.Command, applyMsg.CommandIndex, applyMsg.SnapshotValid, applyMsg.SnapshotTerm, applyMsg.SnapshotIndex)
}

func (rf *Raft) runReplicaCounter() {
	for !rf.killed() {
		if _, isLeader := rf.getLeaderInfo(); !isLeader {
			return
		}

		currentTerm := rf.getCurrentTerm()
		lastLogIndex := rf.getLogSizeWithSnapshotInfo() - 1
		for curIdx := rf.getServerCommitIndex() + 1; curIdx <= lastLogIndex; curIdx++ {
			entry, err := rf.getLogEntryWithSnapshotInfo(curIdx)
			if err != nil || entry.Term != currentTerm {
				continue // Raft can't commit logs from previous term
			}

			count := 1 // self
			for peer := range rf.peers {
				if peer != rf.me && rf.getMatchIndexForPeer(peer) >= curIdx {
					count++
				}
			}

			if count > len(rf.peers)/2 {
				Debug(dLeader, "Server %d replicated log to majority, commits log %d (term=%d)", rf.me, curIdx, entry.Term)
				rf.setServerCommitIndex(curIdx)
				rf.applyCond.Signal()
			}
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// Apply log if the log has been replicated to majority of server
func (rf *Raft) runApplier() {
	Debug(dInfo, "Server %d started applier", rf.me)

	for !rf.killed() {
		var snapshotMsg *ApplyMsg
		var entriesToApply []ApplyMsg

		rf.mu.Lock()
		for rf.pendingSnapshotApplyMsg == nil && rf.logState.lastAppliedIndex >= rf.logState.commitIndex {
			rf.applyCond.Wait()
		}

		if rf.killed() {
			rf.mu.Unlock()
			return
		}

		// Prepare snapshot to apply
		if rf.pendingSnapshotApplyMsg != nil {
			snapshotMsg = rf.pendingSnapshotApplyMsg
			rf.logState.lastAppliedIndex = snapshotMsg.SnapshotIndex
		}

		// Prepare committed log entries to apply
		for curIndex := rf.logState.lastAppliedIndex + 1; curIndex <= rf.logState.commitIndex; curIndex++ {
			realIndex := rf.getRealIndex(curIndex)
			if realIndex < 0 || realIndex >= len(rf.logs) {
				Debug(dError, "Server %d can't get index %d from log, log size %d", rf.me, curIndex, len(rf.logs))
				continue
			}

			if rf.pendingSnapshotApplyMsg != nil && realIndex <= rf.pendingSnapshotApplyMsg.SnapshotIndex {
				Debug(dError, "Server %d doesn't need to commit message before snapshot's last message", rf.me)
				continue
			}

			entry := rf.logs[realIndex]
			applyMsg := ApplyMsg{
				CommandValid: true,
				Command:      entry.Command,
				CommandIndex: curIndex,
			}
			entriesToApply = append(entriesToApply, applyMsg)
			rf.logState.lastAppliedIndex = curIndex
		}
		rf.pendingSnapshotApplyMsg = nil
		rf.mu.Unlock()

		// Apply snapshot (outside lock)
		if snapshotMsg != nil {
			Debug(dSnap, "Server %d sent snapshot applyMsg %v", rf.me, snapshotMsg)
			rf.applyCh <- *snapshotMsg
		}

		// Apply log entries (outside lock)
		for _, msg := range entriesToApply {
			Debug(dCommit, "Server %d sent applyMsg %s", rf.me, msg)
			rf.applyCh <- msg
		}
		Debug(dCommit, "Server %d logState: %v", rf.me, rf.getLogState())
	}
}

func (rf *Raft) runLogReplicator(server int) {
	Debug(dLeader, "Server %d started log replicator for server %d", rf.me, server)
	// This boolean indicates if the prev append entry call is successful, if yes, we will
	// send log entries in batch in the following request, this case we can avoid always sending
	// all the remaining logs thus save some bandwidth
	prevSuccess := false
	for !rf.killed() {
		snapshotState := rf.getSnapshotState()
		if rf.getLogSize() == 0 && rf.getMatchIndexForPeer(server) == snapshotState.LastIncludedIndex {
			Debug(dLeader, "Server %d has 0 logs to replicate to peer %d, nextLogIndex: %d, snapshotState: %v [thread: %d]", rf.me, server, rf.getNextIndexForPeer(server), snapshotState, server)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if rf.getLogSize() != 0 {
			Debug(dLeader, "Server %d(term: %d, electionState: %s) has %d logs with lastLogTerm: %d [thread: %d]", rf.me, rf.getCurrentTerm(), rf.getElectionState(), rf.getLogSize(), rf.getLastLogTerm(), server)
		}

		currentTerm := rf.getCurrentTerm()
		// leaderCommitIndex := rf.getServerCommitIndex()
		leaderCommitIndex := rf.getServerCommitIndex()
		// nextIndex := min(rf.getLogSize(), rf.getNextIndexForPeer(server))
		nextIndex := min(rf.getLogSizeWithSnapshotInfo(), rf.getNextIndexForPeer(server))
		prevLogIndex := nextIndex - 1

		// prevLog, err := rf.getLogEntry(prevLogIndex)
		prevLog, err := rf.getLogEntryWithSnapshotInfo(prevLogIndex)
		prevLogTerm := prevLog.Term
		if err != nil {
			Debug(dError, "Server %d has less logs for server %d, log size: %d, prevLogIndex: %d, reduce nextIndex to %d [thread: %d]",
				rf.me, server, rf.getLogSize(), prevLogIndex, rf.getSnapshotState().LastIncludedIndex+1, server)
			rf.installSnapshot(server)
			continue
		}

		if nextIndex >= rf.getLogSizeWithSnapshotInfo() {
			Debug(dLeader, "Server %d has no logs to replicate for server %d", rf.me, server)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// logEntries, err := rf.getLogEntriesFromIndex(nextIndex)
		logEntries, err := rf.getLogEntriesFromIndexWithSnapshotInfo(nextIndex)
		if err != nil {
			Debug(dError, "Server %d has less logs for server %d, log size: %d, reduce nextIndex to %d [thread: %d]", rf.me, server, rf.getLogSize(), rf.getLogSizeWithSnapshotInfo(), server)
			rf.setNextIndexForPeer(server, rf.getLogSizeWithSnapshotInfo())
			continue
		}

		if !prevSuccess {
			logEntries = []LogEntry{logEntries[0]}
		}

		appendEntriesArgs := AppendEntriesArgs{currentTerm, rf.me, logEntries, prevLogIndex, prevLogTerm, leaderCommitIndex}
		appendEntriesReply := AppendEntriesReply{}

		Debug(dLeader, "Server %d try to replicate log %v to server %d", rf.me, appendEntriesArgs, server)
		// ok := rf.sendAppendEntries(server, &appendEntriesArgs, &appendEntriesReply)
		// if !ok {
		// 	Debug(dLog, "Server %d rpc call to server %d failed, current server role is %s", rf.me, server, roleMap[rf.GetRole()])
		// 	continue
		// }

		// Stop log replicator is current peer is not leader anymore, we haver to put this check after we build the appendEntriesArgs request
		// this way we make sure we only send out request with the corrrect leader information
		// If we put the check at the beginning then we might generate the appendEntries when the server is not leader anymore, leading
		// to problems in log replication
		if _, isLeader := rf.getLeaderInfo(); !isLeader {
			Debug(dLeader, "Server %d is not leader anymore, will not replicate logs [thread: %d]", rf.me, server)
			return
		}

		replyCh := make(chan bool, 1)
		go func() {
			ok := rf.sendAppendEntries(server, &appendEntriesArgs, &appendEntriesReply)
			replyCh <- ok
		}()

		select {
		case ok := <-replyCh:
			if !ok {
				Debug(dLog, "Server %d rpc call to server %d failed, current server role is %s", rf.me, server, roleMap[rf.GetRole()])
				continue
			}
			// Timeout after 1.5 seconds
		case <-time.After(1500 * time.Millisecond):
			Debug(dWarn, "Server %d rpc call to server %d timed out", rf.me, server)
			continue
		}

		// Leader is not with the highest term, step down to follower
		if appendEntriesReply.Term > currentTerm {
			Debug(dWarn, "Server %d(term: %d, electionState: %v) is not leader anymore cuz there is a peer has a higher term", rf.me, rf.getCurrentTerm(), rf.getElectionState())
			rf.setState(FOLLOWER, appendEntriesReply.Term, -1)
			continue
		}

		// Couldn't replicate message, log mismatch found from peer, reduce nextIndex and retry
		if !appendEntriesReply.Success {
			Debug(dWarn, "Server %d couldn't replicate log to server %d, response %v", rf.me, server, appendEntriesReply)
			if appendEntriesReply.ConflictEntryTerm != -1 {
				conflictEntryIndex, conflictEntryTerm := appendEntriesReply.ConflictEntryIndex, appendEntriesReply.ConflictEntryTerm
				curIdx := rf.getLogSizeWithSnapshotInfo() - 1
				for curIdx >= 0 {
					entry, err := rf.getLogEntryWithSnapshotInfo(curIdx)
					if err != nil {
						// Defensive fallback: log entry unexpectedly not found
						Debug(dError, "Server %d failed to get log entry at index %d: %v", rf.me, curIdx, err)
						break
					}
					if entry.Term == conflictEntryTerm {
						break
					}
					curIdx -= 1
				}

				if curIdx >= 0 {
					rf.setNextIndexForPeer(server, curIdx+1)
				} else {
					rf.setNextIndexForPeer(server, conflictEntryIndex)
				}
			} else {
				rf.setNextIndexForPeer(server, nextIndex-1)
			}

			if rf.getNextIndexForPeer(server) >= appendEntriesReply.PeerLogSize {
				rf.setNextIndexForPeer(server, appendEntriesReply.PeerLogSize)
			}
			Debug(dWarn, "Server %d couldn't replicate log to server %d, reduce nextIndex to %d", rf.me, server, rf.getNextIndexForPeer(server))
			prevSuccess = false
			continue
		}

		// Message replication succeeded, update nextIndex and matchIndex
		// note that we are sending message in batch so we need to update nextIndex and matchIndex correspondingly
		Debug(dCommit, "Server %d replicated log %d to server %d, update next index to %d", rf.me, nextIndex+len(logEntries)-1, server, nextIndex+len(logEntries))
		rf.setMatchIndexForPeer(server, nextIndex+len(logEntries)-1)
		rf.setNextIndexForPeer(server, nextIndex+len(logEntries))
		prevSuccess = true
	}
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)

	return ok
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	// Your code here (3A, 3B).
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// could be a heartbeat message from a leader
	if len(args.Entries) == 0 {
		Debug(dInfo, "Server %d(term: %d) with role %s received heartbeat from leader id %d with args %v\n",
			rf.me, rf.electionState.CurrentTerm, roleMap[rf.electionState.Role], args.LeaderId, args)
	} else {
		Debug(dInfo, "Server %d(term: %d) role %s received logs from leader id %d with args %v\n",
			rf.me, rf.electionState.CurrentTerm, roleMap[rf.electionState.Role], args.LeaderId, args)
	}

	// not a heart beat message, processing logs from append entries request
	reply.Success = false
	reqTerm := args.Term
	reply.Term = reqTerm
	reply.ConflictEntryIndex = -1
	reply.ConflictEntryTerm = -1
	reply.PeerLogSize = len(rf.logs) + rf.snapshotState.LastIncludedIndex + 1

	// leaderId := args.LeaderId
	entries := args.Entries
	// this is the index assume the log is never truncated
	virtualPrevLogIndex := args.PrevLogIndex
	prevLogTermFromLeader := args.PrevLogTerm
	leaderCommitIndex := args.LeaderCommit

	// get the actual index in current logs
	realPrevLogIndex := rf.getRealIndex(virtualPrevLogIndex)

	// reply false if term < current term
	if reqTerm < rf.electionState.CurrentTerm {
		Debug(dWarn, "Server %d's term %d is greater than %d's term %d\n", rf.me, rf.electionState.CurrentTerm, args.LeaderId, reqTerm)
		reply.Term = rf.electionState.CurrentTerm

		return
	}

	if reqTerm > rf.electionState.CurrentTerm {
		Debug(dWarn, "Server %d's(electionState: %s) term %d is smaller than %d's term %d\n", rf.me, rf.electionState, rf.electionState.CurrentTerm, args.LeaderId, reqTerm)
		rf.electionState.VotedFor = -1
		rf.electionState.Role = FOLLOWER
		rf.electionState.CurrentTerm = args.Term
		rf.persist()
	}

	// only update the last communication time if we received a heartbeat from a current leader(which carries a term which is at least larger than peer's)
	rf.lastCommTime = time.Now()
	if len(entries) != 0 {
		// prev log index is pointing to an entry that exist in snapshot and is not the last entry in the snapshot
		if realPrevLogIndex < -1 {
			Debug(dWarn, "Server %d's has log size: %d, realPrevLogIndex %d, server snapshotState %v\n", rf.me, len(rf.logs), realPrevLogIndex, rf.snapshotState)
			// let server back down for one index
			reply.ConflictEntryIndex = rf.snapshotState.LastIncludedIndex
			reply.ConflictEntryTerm = rf.snapshotState.LastIncludedTerm
			return
		}

		if realPrevLogIndex >= len(rf.logs) {
			Debug(dWarn, "Server %d's has log size: %d, realPrevLogIndex %d\n", rf.me, len(rf.logs), realPrevLogIndex)
			reply.ConflictEntryIndex = len(rf.logs) + rf.snapshotState.LastIncludedIndex
			reply.ConflictEntryTerm = rf.snapshotState.LastIncludedTerm
			if len(rf.logs) != 0 {
				reply.ConflictEntryTerm = rf.logs[len(rf.logs)-1].Term
			}
			return
		}

		// reply false if log doesn't contain an entry at prevLogIndex whose term matches prevLogTerm
		var prevLogTermOnServer int
		// prevLogIndex is the same as lastIncludedIndex
		if realPrevLogIndex == -1 {
			prevLogTermOnServer = rf.snapshotState.LastIncludedTerm
		} else {
			prevLogTermOnServer = rf.logs[realPrevLogIndex].Term
		}

		if realPrevLogIndex >= 0 && prevLogTermOnServer != prevLogTermFromLeader {
			Debug(dWarn, "Server %d's term at prevLogIndex %d doesn't match prevLogTerm %d\n", rf.me, realPrevLogIndex, prevLogTermFromLeader)
			index := realPrevLogIndex
			for index < len(rf.logs) && index > 0 && rf.logs[index].Term != prevLogTermFromLeader {
				index -= 1
			}

			if 0 <= index && index+1 < len(rf.logs) {
				reply.ConflictEntryIndex = index + 1
				reply.ConflictEntryTerm = rf.logs[index+1].Term
			}

			return
		}

		//
		//    Log truncate and append
		//
		// Truncate log if prevLogIndex is not the last log in current peer's log.This could happen during a split brain scenario.
		// Note that we should not truncte committed log as once log is committed it it durable
		Debug(dLog, "Server %d received log entries %s\n", rf.me, args)
		// we might need to truncate log if there is any mistmatch for log entry at the same index
		// Don't just blindly truncate like rf.logs = rf.logs[:realPrevLogIndex+1] as an out of order message could incorrectly truncate
		// message
		// we should not change committed entry, we should only check entries that are not committed
		if realPrevLogIndex+1 <= len(rf.logs) {
			Debug(dPersist, "Server %d log length before append: %d", rf.me, len(rf.logs))
			misMatchIndex := -1
			entryIndex := 0
			for idx := realPrevLogIndex + 1; idx < len(rf.logs) && entryIndex < len(entries); idx += 1 {
				if rf.logs[idx].Term != entries[idx-realPrevLogIndex-1].Term {
					misMatchIndex = idx
					break
				}
				entryIndex += 1
			}

			if misMatchIndex != -1 {
				rf.logs = rf.logs[:misMatchIndex]
				Debug(dLog, "Server %d truncated its log to index %d", rf.me, misMatchIndex)
			}
		}

		// If there were retries for a request, we just do an idempotenet operation here, we are put the entry
		// at the same index it was supposed to be at. For example, if prevLogIndex was 5, then the first entry in the
		// input should be at index 6. Note that we already made sure the prevLogTerm matches with the log entry's term
		// at prevLogIndex
		idx := realPrevLogIndex + 1
		for idx < len(rf.logs) && idx-realPrevLogIndex-1 < len(entries) {
			rf.logs[idx] = entries[idx-realPrevLogIndex-1]
			idx += 1
		}
		// append new logs
		if idx-realPrevLogIndex-1 < len(entries) {
			rf.logs = append(rf.logs, entries[idx-realPrevLogIndex-1:]...)
		}

		Debug(dPersist, "Server %d log length after append: %d", rf.me, len(rf.logs))
		Debug(dLog, "Server %d append entries in log\n", rf.me)
	}

	// Debug(dWarn, "Server %d's commitIndex %d is behind leader %d's commitIndex %d, server log count: %d\n", rf.me, rf.logState.commitIndex, args.LeaderId, leaderCommitIndex, len(rf.logs))
	// This check should happen after any potential log truncate that will happen during an appentry behavior
	// Otherwise we could end up mistakenly updating commit index on current peer
	if leaderCommitIndex > rf.logState.commitIndex {
		Debug(dWarn, "Server %d's commitIndex %d is behind leader %d's commitIndex %d, server log count: %d, server last includedIndex %d, virtual log index %d\n",
			rf.me, rf.logState.commitIndex, args.LeaderId, leaderCommitIndex, len(rf.logs), rf.snapshotState.LastIncludedIndex, len(rf.logs)+rf.snapshotState.LastIncludedIndex)
		newCommitIndex := rf.logState.commitIndex
		// Find last new entry from current term, we should only commit the entry that belongs to current term
		// For example,
		// The original leader (Term 2) appends a log entry at index 4, but it fails to replicate it to a majority of followers.
		// The original leader crashes or gets partitioned.
		// The remaining two followers elect a new leader (Term 3).
		// The new leader writes a new log entry at index 4 (with Term 3), effectively overwriting the uncommitted entry from the old leader.
		// The new leader commits up to index 4 after successfully replicating it to a majority.
		// The old leader rejoins the cluster and steps down as a follower. It receives an AppendEntries RPC(heartbeat) from the new leader with commitIndex = 4.
		// The old leader blindly sets its commit index to 4, without checking whether its log at index 4 matches the leader’s log term (Term 3).
		// As a result, the old leader incorrectly considers its own log at index 4 (Term 2) as committed, violating Raft’s safety property.
		// So we should just commit entry from current term
		for i := rf.logState.commitIndex + 1; i <= min(leaderCommitIndex, len(rf.logs)+rf.snapshotState.LastIncludedIndex); i++ {
			if rf.logs[i-rf.snapshotState.LastIncludedIndex-1].Term != rf.electionState.CurrentTerm {
				continue
			}
			newCommitIndex = i
		}
		rf.logState.commitIndex = newCommitIndex
		Debug(dWarn, "Server %d's commitIndex %d, lastAppliedIndex %d, server snapshotState %v, logSize: %d",
			rf.me, rf.logState.commitIndex, rf.logState.lastAppliedIndex, rf.snapshotState, len(rf.logs))
		rf.applyCond.Signal()
	}

	if len(rf.logs) == 0 {
		Debug(dLog, "Server %d log size: %d", rf.me, len(rf.logs))
	} else {
		Debug(dLog, "Server %d log size: %d lastLogTerm: %d, logs: %v\n", rf.me, len(rf.logs), rf.logs[len(rf.logs)-1].Term, rf.logs)
	}
	rf.persist()

	reply.Term = rf.electionState.CurrentTerm
	reply.Success = true
}

func (rf *Raft) appendLogLocally(logEntry LogEntry) int {
	rf.logs = append(rf.logs, logEntry)
	rf.persist()

	rf.peerIndexState.nextIndex[rf.me] = len(rf.logs) + rf.snapshotState.LastIncludedIndex + 1
	rf.peerIndexState.matchIndex[rf.me] = len(rf.logs) + rf.snapshotState.LastIncludedIndex
	Debug(dLog, "Server %d appended log %v and has log size %d", rf.me, logEntry, len(rf.logs))

	return len(rf.logs) + rf.snapshotState.LastIncludedIndex
}
