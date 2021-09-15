package agentpool

import (
	"time"

	"github.com/forta-network/forta-node/clients"
	"github.com/forta-network/forta-node/clients/agentgrpc"
	"github.com/forta-network/forta-node/clients/messaging"
	"github.com/forta-network/forta-node/config"
	"github.com/forta-network/forta-node/protocol"
	"github.com/forta-network/forta-node/services/scanner"
	log "github.com/sirupsen/logrus"
)

// AgentPool maintains the pool of agents that the scanner should
// interact with.
type AgentPool struct {
	agents       []*Agent
	txResults    chan *scanner.TxResult
	blockResults chan *scanner.BlockResult
	msgClient    clients.MessageClient
	dialer       func(config.AgentConfig) clients.AgentClient
}

// NewAgentPool creates a new agent pool.
func NewAgentPool(msgClient clients.MessageClient) *AgentPool {
	agentPool := &AgentPool{
		txResults:    make(chan *scanner.TxResult, DefaultBufferSize),
		blockResults: make(chan *scanner.BlockResult, DefaultBufferSize),
		msgClient:    msgClient,
		dialer: func(ac config.AgentConfig) clients.AgentClient {
			client := agentgrpc.NewClient()
			client.MustDial(ac)
			return client
		},
	}
	agentPool.registerMessageHandlers()
	go agentPool.logAgentChanBuffersLoop()
	return agentPool
}

// SendEvaluateTxRequest sends the request to all of the active agents which
// should be processing the block.
func (ap *AgentPool) SendEvaluateTxRequest(req *protocol.EvaluateTxRequest) {
	log.WithField("tx", req.Event.Transaction.Hash).Debug("SendEvaluateTxRequest")
	agents := ap.agents
	for _, agent := range agents {
		if !agent.ready || !agent.shouldProcessBlock(req.Event.Block.BlockNumber) {
			log.WithFields(log.Fields{
				"agent": agent.config.ID,
				"ready": agent.ready,
			}).Debug("agent not ready, NOT sending block request")
			continue
		}
		log.WithField("agent", agent.config.ID).Debug("sending tx request to evalBlockCh")
		writeToTxChannel(agent.evalTxCh, req)
	}
	log.WithField("tx", req.Event.Transaction.Hash).Debug("Finished SendEvaluateTxRequest")
}

// TxResults returns the receive-only tx results channel.
func (ap *AgentPool) TxResults() <-chan *scanner.TxResult {
	return ap.txResults
}

func writeToTxChannel(evalCh chan *protocol.EvaluateTxRequest, req *protocol.EvaluateTxRequest) {
	defer func() {
		if err := recover(); err != nil {
			log.Warn("attempt write to closed tx agent channel, ignoring")
		}
	}()
	evalCh <- req
}

func writeToBlockChannel(evalCh chan *protocol.EvaluateBlockRequest, req *protocol.EvaluateBlockRequest) {
	defer func() {
		if err := recover(); err != nil {
			log.Warn("attempt write to closed block agent channel, ignoring")
		}
	}()
	evalCh <- req
}

// SendEvaluateBlockRequest sends the request to all of the active agents which
// should be processing the block.
func (ap *AgentPool) SendEvaluateBlockRequest(req *protocol.EvaluateBlockRequest) {
	log.WithField("block", req.Event.BlockNumber).Debug("SendEvaluateBlockRequest")
	agents := ap.agents
	for _, agent := range agents {
		if !agent.ready || !agent.shouldProcessBlock(req.Event.BlockNumber) {
			log.WithFields(log.Fields{
				"agent": agent.config.ID,
				"ready": agent.ready,
			}).Debug("agent not ready, NOT sending block request")
			continue
		}
		log.WithField("agent", agent.config.ID).Debug("sending block request to evalBlockCh")
		writeToBlockChannel(agent.evalBlockCh, req)
	}
	log.WithField("block", req.Event.BlockNumber).Debug("Finished SendEvaluateBlockRequest")
}

func (ap *AgentPool) logAgentChanBuffersLoop() {
	ticker := time.NewTicker(time.Second * 30)
	for range ticker.C {
		ap.logAgentChanBuffers()
	}
}

func (ap *AgentPool) logAgentChanBuffers() {
	log.Debug("logAgentChanBuffers")
	for _, agent := range ap.agents {
		log.WithFields(log.Fields{
			"agent":         agent.config.ID,
			"buffer-blocks": len(agent.evalBlockCh),
			"buffer-txs":    len(agent.evalTxCh),
		}).Debug("agent request channel buffers")
	}
}

// BlockResults returns the receive-only tx results channel.
func (ap *AgentPool) BlockResults() <-chan *scanner.BlockResult {
	return ap.blockResults
}

func (ap *AgentPool) handleAgentVersionsUpdate(payload messaging.AgentPayload) error {
	log.Debug("handleAgentVersionsUpdate")
	latestVersions := payload

	// The agents list which we completely replace with the old ones.
	var newAgents []*Agent

	// Find the missing agents in the pool, add them to the new agents list
	// and send a "run" message.
	var agentsToRun []config.AgentConfig
	for _, agentCfg := range latestVersions {
		var found bool
		for _, agent := range ap.agents {
			found = found || (agent.config.ContainerName() == agentCfg.ContainerName())
		}
		if !found {
			newAgents = append(newAgents, NewAgent(agentCfg, ap.msgClient, ap.txResults, ap.blockResults))
			agentsToRun = append(agentsToRun, agentCfg)
		}
	}

	// Find the missing agents in the latest versions and send a "stop" message.
	// Otherwise, add to the new agents list so we keep on running.
	var agentsToStop []config.AgentConfig
	for _, agent := range ap.agents {
		var found bool
		var agentCfg config.AgentConfig
		for _, agentCfg = range latestVersions {
			found = found || (agent.config.ContainerName() == agentCfg.ContainerName())
			if found {
				break
			}
		}
		if !found {
			agent.Close()
			agent.ready = false
			agentsToStop = append(agentsToStop, agent.config)
		} else {
			newAgents = append(newAgents, agent)
		}
	}

	ap.agents = newAgents
	if len(agentsToRun) > 0 {
		ap.msgClient.Publish(messaging.SubjectAgentsActionRun, agentsToRun)
	}
	if len(agentsToStop) > 0 {
		ap.msgClient.Publish(messaging.SubjectAgentsActionStop, agentsToStop)
	}
	return nil
}

func (ap *AgentPool) handleStatusRunning(payload messaging.AgentPayload) error {
	log.Debug("handleStatusRunning")
	// If an agent was added before and just started to run, we should mark as ready.
	for _, agentCfg := range payload {
		for _, agent := range ap.agents {
			if agent.config.ContainerName() == agentCfg.ContainerName() {
				agent.setClient(ap.dialer(agent.config))
				agent.ready = true
				go agent.processTransactions()
				go agent.processBlocks()
			}
		}
	}
	return nil
}

func (ap *AgentPool) handleStatusStopped(payload messaging.AgentPayload) error {
	log.Debug("handleStatusStopped")
	var newAgents []*Agent
	for _, agent := range ap.agents {
		var stopped bool
		for _, agentCfg := range payload {
			if agent.config.ContainerName() == agentCfg.ContainerName() {
				log.WithField("agent", agent.config.ID).Debug("stopping")
				agent.Close()
				agent.ready = false
				stopped = true
				break
			}
		}
		if !stopped {
			log.WithField("agent", agent.config.ID).Debug("not stopped")
			newAgents = append(newAgents, agent)
		}
	}
	ap.agents = newAgents
	return nil
}

func (ap *AgentPool) registerMessageHandlers() {
	ap.msgClient.Subscribe(messaging.SubjectAgentsVersionsLatest, messaging.AgentsHandler(ap.handleAgentVersionsUpdate))
	ap.msgClient.Subscribe(messaging.SubjectAgentsStatusRunning, messaging.AgentsHandler(ap.handleStatusRunning))
	ap.msgClient.Subscribe(messaging.SubjectAgentsStatusStopped, messaging.AgentsHandler(ap.handleStatusStopped))
}
