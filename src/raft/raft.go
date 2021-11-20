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
	"labrpc"
	"math/rand"
	"sync"
	"time"
)

type Request struct {
	command interface{}
	index   int
}

type ApplyMsg struct {
	Index       int
	Command     interface{}
	UseSnapshot bool   // ignore for Assignment2; only used in Assignment3
	Snapshot    []byte // ignore for Assignment2; only used in Assignment3
}

type LogEntry struct {
	Command interface{} // received from client
	Term    int         // term when entry was received
}

type AppendEntriesArgs struct {
	Term         int        // leader term
	LeaderID     int        // leader id
	PrevLogIndex int        // index of log entry immediately preceding new ones
	PrevLogTerm  int        // term of prevLogIndex entry
	Entries      []LogEntry // log entries to store (empty for heartbeat; consider sending more than one for efficiency)
	LeaderCommit int        // highest log index known to be committed by the leader
}

type AppendEntriesReply struct {
	Term    int  // receiver term
	Success bool // does follower contain entry matching prevLogIndex and prevLogTerm
}

type RequestVoteArgs struct {
	Term         int // candidate term
	CandidateID  int // candidate id
	LastLogIndex int // index of candidate's last log entry
	LastLogTerm  int // term of candidate's last log entry
}

type RequestVoteReply struct {
	Term        int  // receiver term
	VoteGranted bool // true if vote granted false otherwise
}

/*a single RAFT peer object*/
type Raft struct {
	mu        sync.Mutex
	peers     []*labrpc.ClientEnd
	persister *Persister
	me        int // index into peers[]

	// persistent state
	currentTerm int        // latest term that this server has seen
	votedFor    int        // candidate id of the server that this server voted for in current term
	state       string     // whether the node is a follower, candidate, or leader
	log         []LogEntry // stores log entries

	// volatile state
	commitIndex int // highest log entry known to be committed
	lastApplied int // highest log entry applied to state machine

	// volatile state if leader
	nextIndex  map[int]int // index of the next entry to send to each server
	matchIndex map[int]int // index of highest log entry known to be replicated on each server

	// utility
	voteCount      int           // count of total votes in each election for a node
	voteRequested  chan int      // channel to inform main process if requestVote RPC received
	heartBeat      chan int      // channel to inform main process if heartbeat received
	legitLeader    chan bool     // channel to inform main process of a heartbeat from a legitimate leader
	requestQueue   chan Request  // queue for client requests
	followerCommit chan ApplyMsg // forwards commit requests for follower
}

func Make(peers []*labrpc.ClientEnd, me int, persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	// Your initialization code here.
	rf.state = "follower"
	rf.currentTerm = 0
	rf.votedFor = -1
	rf.voteRequested = make(chan int)
	rf.heartBeat = make(chan int)
	rf.legitLeader = make(chan bool)
	rf.requestQueue = make(chan Request, 500)
	rf.followerCommit = make(chan ApplyMsg)
	rf.lastApplied = 0
	rf.commitIndex = 0
	rf.log = make([]LogEntry, 1)
	rf.log[0].Term = 0 // garbage value at index 0
	rf.nextIndex = make(map[int]int)
	rf.matchIndex = make(map[int]int)

	for i := 0; i < len(rf.peers); i++ {
		if i == rf.me {
			continue
		}
		rf.nextIndex[i] = 1
	}

	for i := 0; i < len(rf.peers); i++ {
		if i == rf.me {
			continue
		}
		rf.matchIndex[i] = 0
	}

	// spawn necessary goroutines
	go handleElection(rf, me)
	go requestHandler(rf, applyCh)
	go applyHandler(rf, applyCh)

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	return rf
}

func (rf *Raft) GetState() (int, bool) {
	var term int
	var isleader bool

	rf.mu.Lock()
	state := rf.state
	term = rf.currentTerm
	rf.mu.Unlock()

	if state == "leader" {
		isleader = true

	} else {
		isleader = false
	}

	return term, isleader
}

func (rf *Raft) persist() {
}

func (rf *Raft) readPersist(data []byte) {
}

func (rf *Raft) Start(command interface{}) (int, int, bool) {
	c := command

	rf.mu.Lock()
	s := rf.state
	rf.mu.Unlock()

	if s == "leader" {
		// add entry to log
		rf.mu.Lock()
		term := rf.currentTerm
		index := len(rf.log)
		rf.mu.Unlock()

		rf.requestQueue <- Request{c, index}

		return index, term, true

	} else {
		return -1, -1, false
	}
}

func (rf *Raft) Kill() {
}

/*periodically checks if a value is committed and in which case dispatches it to applyChannel*/
func applyHandler(rf *Raft, applyCh chan ApplyMsg) {
	for {
		rf.mu.Lock()
		commitIdx := rf.commitIndex
		lastAppl := rf.lastApplied
		log := rf.log
		rf.mu.Unlock()

		// apply log entry if it has been committed
		if commitIdx > lastAppl {
			rf.mu.Lock()
			rf.lastApplied++
			lastAppl = rf.lastApplied
			rf.mu.Unlock()

			applyCh <- ApplyMsg{lastAppl, log[lastAppl].Command, false, make([]byte, 0)}
		}
	}
}

/*handles client request (agreement on new log entry)*/
func requestHandler(rf *Raft, applyCh chan ApplyMsg) {
	for {
		newReq := <-rf.requestQueue

		// retrieve required state
		rf.mu.Lock()
		term := rf.currentTerm
		entry := LogEntry{newReq.command, term}
		rf.log = append(rf.log, entry)
		log := rf.log
		totalPeers := len(rf.peers)
		rf.mu.Unlock()

		// replicate new entry
		storeReplies := make([]*AppendEntriesReply, totalPeers)
		successCount := 0

		// send append entries
		for i := 0; i < totalPeers; i++ {
			rf.mu.Lock()
			nextIndex := rf.nextIndex[i]
			rf.mu.Unlock()

			if (i != rf.me) && (len(log)-1 >= nextIndex) {
				storeReplies[i] = new(AppendEntriesReply)

				prevLogIndex := nextIndex - 1
				Args := AppendEntriesArgs{term, rf.me, prevLogIndex, log[prevLogIndex].Term, log, rf.lastApplied}
				rf.sendAppendEntries(i, Args, storeReplies[i]) // blocks

				if storeReplies[i].Success {
					rf.mu.Lock()
					rf.matchIndex[i] += 1
					rf.nextIndex[i] = rf.matchIndex[i] + 1
					rf.mu.Unlock()

					successCount++
				}
			}
		}

		// ignore if entry not committed
		if successCount > (totalPeers / 2) {
			// commit entry
			rf.mu.Lock()
			rf.commitIndex = newReq.index
			rf.mu.Unlock()
		}
	}
}

func validateVote(lastLogIndex1, lastLogIndex2, lastLogTerm1, lastLogTerm2 int) bool {
	/*
		returns true if candidate's log is at least
		as up-to-date as the follower's log
		otherwise returns false

		Input:
		1: Candidate
		2: Follower
	*/

	if lastLogTerm1 > lastLogTerm2 {
		return true

	} else if lastLogTerm1 < lastLogTerm2 {
		return false

	} else if lastLogIndex1 >= lastLogIndex2 {
		return true

	} else {
		return false
	}
}

/*request vote rpc received and reply dispatched*/
func (rf *Raft) RequestVote(args RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	currentTerm := rf.currentTerm
	votedFor := rf.votedFor
	lastLogIndex := len(rf.log) - 1
	lastLogTerm := rf.log[lastLogIndex].Term
	rf.mu.Unlock()

	// candidate requesting vote has a stale term
	// or the same term in which case
	// this node is the candidate itself or
	// has already voted for another candidate
	if currentTerm >= args.Term {
		reply.Term = currentTerm
		reply.VoteGranted = false

	} else {
		reply.VoteGranted = false

		rf.mu.Lock()
		rf.currentTerm = args.Term
		rf.state = "follower"
		rf.votedFor = args.CandidateID
		rf.mu.Unlock()

		// check if log atleast up-to-date as its own
		validate := validateVote(args.LastLogIndex, lastLogIndex, args.LastLogTerm, lastLogTerm)
		if (votedFor == -1 || votedFor == args.CandidateID) && validate {
			rf.voteRequested <- -1
			reply.VoteGranted = true
		}
	}
}

/*append entries rpc received and reply dispatched*/
func (rf *Raft) AppendEntries(args AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	currentTerm := rf.currentTerm
	rf.mu.Unlock()

	if currentTerm > args.Term {
		// received appendEntries from old leader
		// return newer term
		reply.Term = currentTerm
		reply.Success = false

	} else {
		// heartBeat received
		rf.mu.Lock()
		rf.currentTerm = args.Term
		rf.state = "follower"

		// if candidate and heartbeat received from legitLeader leader
		// then cancel election and revert to follower
		if rf.state == "candidate" {
			rf.legitLeader <- false
		}

		rf.mu.Unlock()

		// reset timer
		rf.heartBeat <- 1

		// append new entries
		rf.mu.Lock()
		for i := args.PrevLogIndex + 1; i < len(args.Entries); i++ {
			if i >= 0 {
				rf.log = append(rf.log, args.Entries[i])
			}
		}

		// if any new entries have been committed by the leader
		if args.LeaderCommit > rf.commitIndex {
			rf.commitIndex = min(args.LeaderCommit, len(rf.log)-1)
		}

		rf.mu.Unlock()

		reply.Success = true
	}
}

/*request vote rpc sent and reply handled*/
func (rf *Raft) sendRequestVote(server int, args RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)

	// handle reply for the requested vote
	if ok {
		rf.mu.Lock()
		if reply.VoteGranted {
			rf.voteCount += 1

		} else {
			// consider converting candidate to follower
			if rf.currentTerm < reply.Term {
				rf.state = "follower"
			}
			rf.currentTerm = reply.Term
		}
		rf.mu.Unlock()
	}

	return ok
}

/*append entries rpc sent and reply handled*/
func (rf *Raft) sendAppendEntries(server int, args AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)

	// handle appendEntries reply from a node
	if ok {
		rf.mu.Lock()
		// leader returns to follower state if its currentTerm is stale
		if rf.currentTerm < reply.Term {
			rf.currentTerm = reply.Term
			rf.state = "follower"
		}
		rf.mu.Unlock()

	} else {
		rf.mu.Lock()
		rf.state = "follower"
		rf.mu.Unlock()
	}

	return ok
}

/*return false when timer runs out*/
func timer(timeout chan bool, heartBeat bool) bool {
	if !heartBeat {
		rand.Seed(time.Now().UnixNano())
		min := 450
		max := 600
		dur := rand.Intn(max-min) + min

		time.Sleep(time.Duration(dur) * time.Millisecond)

		timeout <- false
		return false

	} else {
		rand.Seed(time.Now().UnixNano())
		time.Sleep(100 * time.Millisecond)

		return false
	}
}

/*sends empty AppendEntries RPC as heartbeat message*/
func heartBeat(rf *Raft) {
	// get state
	rf.mu.Lock()
	totalPeers := len(rf.peers)
	term := rf.currentTerm
	log := rf.log
	rf.mu.Unlock()

	storeReplies := make(map[int]*AppendEntriesReply)

	// send heartbeats
	for i := 0; i < totalPeers; i++ {
		if i != rf.me {
			rf.mu.Lock()
			prevLogIndex := rf.nextIndex[i] - 1
			Args := AppendEntriesArgs{term, rf.me, prevLogIndex, log[prevLogIndex].Term, []LogEntry{}, rf.lastApplied}
			rf.mu.Unlock()

			storeReplies[i] = new(AppendEntriesReply)
			go rf.sendAppendEntries(i, Args, storeReplies[i])
		}
	}
}

/*monitors node state and calls election on detecting leader failure*/
func handleElection(rf *Raft, me int) {
	rand.Seed(time.Now().UnixNano())
	min := 450
	max := 600
	prevState := "follower"
	heartbeatCount := 0

	for {
		rf.mu.Lock()
		state := rf.state
		rf.mu.Unlock()

		// reset vote
		if (prevState == "candidate" && state == "follower") || heartbeatCount == 0 {
			rf.mu.Lock()
			rf.votedFor = -1
			rf.mu.Unlock()
		}

		switch state {
		case "follower":
			select {
			case <-time.After(time.Duration(rand.Intn(max-min)+min) * time.Millisecond):
				// leader timed out
				heartbeatCount = 0
				rf.mu.Lock()
				rf.state = "candidate"
				rf.mu.Unlock()
			case <-rf.heartBeat:
				// timeout reset
				heartbeatCount++
			case <-rf.voteRequested:
				// timeout reset
				heartbeatCount = 0
			}
		case "candidate":
			election(rf)
		case "leader":
			heartBeat(rf)
			t := make(chan bool)
			h := true
			timer(t, h)
		}
	}
}

/*election process conducted upon leader failure*/
func election(rf *Raft) {
	rf.mu.Lock()
	rf.currentTerm += 1
	term := rf.currentTerm
	rf.voteCount += 1
	totalPeers := len(rf.peers)
	lastLogIndex := len(rf.log) - 1
	lastLogTerm := rf.log[lastLogIndex].Term
	rf.mu.Unlock()

	storeReplies := make(map[int]*RequestVoteReply)
	Args := RequestVoteArgs{term, rf.me, lastLogIndex, lastLogTerm}

	// request votes
	for i := 0; i < totalPeers; i++ {
		if i != rf.me {
			storeReplies[i] = new(RequestVoteReply)
			go rf.sendRequestVote(i, Args, storeReplies[i])
		}
	}

	// wait for election to complete or timeout
	loop := true
	ch := make(chan bool)
	h := false
	go timer(ch, h)

	for loop {
		select {
		case loop = <-ch:
			continue
		case loop = <-rf.legitLeader:
			continue
		default:
			rf.mu.Lock()
			status := rf.voteCount
			rf.mu.Unlock()

			if status > (totalPeers / 2) {
				// become Leader
				rf.mu.Lock()
				drainQueue(rf.requestQueue)

				// reinitialize after election
				for i := 0; i < len(rf.peers); i++ {
					if i == rf.me {
						continue
					}
					rf.nextIndex[i] = 1
				}

				for i := 0; i < len(rf.peers); i++ {
					if i == rf.me {
						continue
					}
					rf.matchIndex[i] = 0
				}

				rf.state = "leader"
				rf.votedFor = -1
				rf.mu.Unlock()

				loop = false
				continue
			}
		}
	}

	rf.mu.Lock()
	rf.voteCount = 0
	rf.mu.Unlock()
}

/*utility functions*/
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func drainQueue(ch chan Request) {
	/*drains client request queue channel*/
	for len(ch) > 0 {
		<-ch
	}
}
