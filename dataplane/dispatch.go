// Copyright 2021-2022 The httpmq Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dataplane

import (
	"context"
	"fmt"
	"sync"

	"github.com/alwitt/httpmq/common"
	"github.com/alwitt/httpmq/core"
	"github.com/apex/log"
	"github.com/nats-io/nats.go"
)

// MessageDispatcher process a consumer subscription request from a client and dispatch
// messages to that client
type MessageDispatcher interface {
	// Start starts operations
	Start(msgOutput ForwardMessageHandlerCB, errorCB AlertOnErrorCB) error
}

// pushMessageDispatcher implements MessageDispatcher for a push consumer
type pushMessageDispatcher struct {
	common.Component
	nats       *core.NatsClient
	optContext context.Context
	wg         *sync.WaitGroup
	lock       *sync.Mutex
	started    bool
	// msgTracking monitors the set of inflight messages
	msgTracking   JetStreamInflightMsgProcessor
	msgTrackingTP common.TaskProcessor
	// ackWatcher monitors for ACK being received
	ackWatcher JetStreamACKReceiver
	// subscriber connected to JetStream to receive messages
	subscriber JetStreamPushSubscriber
}

// GetPushMessageDispatcher get a new push MessageDispatcher
func GetPushMessageDispatcher(
	natsClient *core.NatsClient,
	stream, subject, consumer string,
	deliveryGroup *string,
	maxInflightMsgs int,
	wg *sync.WaitGroup,
	ctxt context.Context,
) (MessageDispatcher, error) {
	instance := fmt.Sprintf("%s@%s/%s", consumer, stream, subject)
	logTags := log.Fields{
		"module":    "dataplane",
		"component": "push-msg-dispatcher",
		"stream":    stream,
		"subject":   subject,
		"consumer":  consumer,
	}
	if ctxt.Value(common.RequestParam{}) != nil {
		v, ok := ctxt.Value(common.RequestParam{}).(common.RequestParam)
		if ok {
			v.UpdateLogTags(logTags)
		}
	}

	// Define components
	ackReceiver, err := getJetStreamACKReceiver(natsClient, stream, subject, consumer)
	if err != nil {
		log.WithError(err).WithFields(logTags).Errorf("Unable to define ACK receiver")
		return nil, err
	}
	msgTrackingTP, err := common.GetNewTaskProcessorInstance(instance, maxInflightMsgs*4, ctxt)
	if err != nil {
		log.WithError(err).WithFields(logTags).Errorf("Unable to define task processor")
		return nil, err
	}
	msgTracking, err := getJetStreamInflightMsgProcessor(
		msgTrackingTP, stream, subject, consumer, ctxt,
	)
	if err != nil {
		log.WithError(err).WithFields(logTags).Errorf("Unable to define MSG tracker")
		return nil, err
	}
	subscriber, err := getJetStreamPushSubscriber(
		natsClient, stream, subject, consumer, deliveryGroup,
	)
	if err != nil {
		log.WithError(err).WithFields(logTags).Errorf("Unable to define MSG subscriber")
		return nil, err
	}

	return &pushMessageDispatcher{
		Component:     common.Component{LogTags: logTags},
		nats:          natsClient,
		optContext:    ctxt,
		wg:            wg,
		lock:          &sync.Mutex{},
		started:       false,
		msgTracking:   msgTracking,
		msgTrackingTP: msgTrackingTP,
		ackWatcher:    ackReceiver,
		subscriber:    subscriber,
	}, nil
}

// Start starts the push message dispatcher operation
func (d *pushMessageDispatcher) Start(
	msgOutput ForwardMessageHandlerCB, errorCB AlertOnErrorCB,
) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.started {
		return fmt.Errorf("already started")
	}

	// Start message tracking TP
	if err := d.msgTrackingTP.StartEventLoop(d.wg); err != nil {
		log.WithError(err).WithFields(d.LogTags).Errorf("Failed to start MSG tracker task processor")
		return err
	}

	// Start ACK receiver
	if err := d.ackWatcher.SubscribeForACKs(
		d.wg, d.optContext, func(ai AckIndication, ctxt context.Context) {
			log.WithFields(d.LogTags).Debugf("Processing %s", ai.String())
			// Pass to message tracker in non-blocking mode
			if err := d.msgTracking.HandlerMsgACK(ai, false, ctxt); err != nil {
				log.WithError(err).WithFields(d.LogTags).Errorf("Failed to submit %s", ai.String())
			}
		},
	); err != nil {
		log.WithError(err).WithFields(d.LogTags).Errorf("Failed to start ACK receiver")
		return err
	}

	// Start subscriber
	if err := d.subscriber.StartReading(func(msg *nats.Msg, ctxt context.Context) error {
		msgName := msgToString(msg)
		log.WithFields(d.LogTags).Debugf("Processing %s", msgName)
		// Forward the message toward consumer
		if err := msgOutput(msg, ctxt); err != nil {
			log.WithError(err).WithFields(d.LogTags).Errorf("Unable to forward %s", msgName)
			return err
		}
		// Pass to message tracker in non-blocking mode
		if err := d.msgTracking.RecordInflightMessage(msg, false, ctxt); err != nil {
			log.WithError(err).WithFields(d.LogTags).Errorf("Unable to record %s", msgName)
			return err
		}
		return nil
	}, errorCB, d.wg, d.optContext); err != nil {
		log.WithError(err).WithFields(d.LogTags).Errorf("Failed to start MSG subscriber")
		return err
	}

	d.started = true
	return nil
}
