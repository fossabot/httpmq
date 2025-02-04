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
	"reflect"
	"time"

	"github.com/alwitt/httpmq/common"
	"github.com/apex/log"
	"github.com/nats-io/nats.go"
)

// JetStreamInflightMsgProcessor processes inflight JetStream messages awaiting ACK
type JetStreamInflightMsgProcessor interface {
	// RecordInflightMessage records a new JetStream message inflight awaiting ACK
	RecordInflightMessage(msg *nats.Msg, blocking bool, callCtxt context.Context) error
	// HandlerMsgACK processes a new message ACK
	HandlerMsgACK(ack AckIndication, blocking bool, callCtxt context.Context) error
}

// perConsumerInflightMessages set of messages awaiting ACK for a consumer
type perConsumerInflightMessages struct {
	inflight map[uint64]*nats.Msg
}

// perStreamInflightMessages set of perConsumerInflightMessages for each consumer
type perStreamInflightMessages struct {
	consumers map[string]*perConsumerInflightMessages
}

// jetStreamInflightMsgProcessorImpl implements JetStreamInflightMsgProcessor
type jetStreamInflightMsgProcessorImpl struct {
	common.Component
	subject, consumer string
	tp                common.TaskProcessor
	inflightPerStream map[string]*perStreamInflightMessages
}

// getJetStreamInflightMsgProcessor define new JetStreamInflightMsgProcessor
func getJetStreamInflightMsgProcessor(
	tp common.TaskProcessor, stream, subject, consumer string, ctxt context.Context,
) (JetStreamInflightMsgProcessor, error) {
	logTags := log.Fields{
		"module":    "dataplane",
		"component": "js-inflight-msg-holdling",
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
	instance := jetStreamInflightMsgProcessorImpl{
		Component:         common.Component{LogTags: logTags},
		subject:           subject,
		consumer:          consumer,
		tp:                tp,
		inflightPerStream: make(map[string]*perStreamInflightMessages),
	}
	// Add handlers
	if err := tp.AddToTaskExecutionMap(
		reflect.TypeOf(jsInflightCtrlRecordNewMsg{}),
		instance.processInflightMessage,
	); err != nil {
		return nil, err
	}
	if err := tp.AddToTaskExecutionMap(
		reflect.TypeOf(jsInflightCtrlRecordACK{}),
		instance.processMsgACK,
	); err != nil {
		return nil, err
	}
	return &instance, nil
}

// =========================================================================

type jsInflightCtrlRecordNewMsg struct {
	timestamp time.Time
	blocking  bool
	message   *nats.Msg
	resultCB  func(err error)
}

// RecordInflightMessage records a new JetStream message inflight awaiting ACK
func (c *jetStreamInflightMsgProcessorImpl) RecordInflightMessage(
	msg *nats.Msg, blocking bool, callCtxt context.Context,
) error {
	resultChan := make(chan error)
	handler := func(err error) {
		resultChan <- err
	}

	request := jsInflightCtrlRecordNewMsg{
		timestamp: time.Now(),
		blocking:  blocking,
		message:   msg,
		resultCB:  handler,
	}

	if err := c.tp.Submit(request, callCtxt); err != nil {
		log.WithError(err).WithFields(c.LogTags).Errorf("Failed to submit %s", msgToString(msg))
		return err
	}

	// Don't wait for a response
	if !blocking {
		return nil
	}

	var err error
	// Wait for the response or timeout
	select {
	case result, ok := <-resultChan:
		if !ok {
			err = fmt.Errorf("response to new inflight message is invalid")
		} else {
			err = result
		}
	case <-callCtxt.Done():
		err = callCtxt.Err()
	}

	if err != nil {
		log.WithError(err).WithFields(c.LogTags).Errorf("Processing %s failed", msgToString(msg))
	}
	return err
}

// processInflightMessage support TaskProcessor, handle jsInflightCtrlRecordNewMsg
func (c *jetStreamInflightMsgProcessorImpl) processInflightMessage(param interface{}) error {
	request, ok := param.(jsInflightCtrlRecordNewMsg)
	if !ok {
		return fmt.Errorf(
			"can not process unknown type %s for record inflight message",
			reflect.TypeOf(param),
		)
	}
	err := c.ProcessInflightMessage(request.message)
	if request.blocking {
		request.resultCB(err)
	}
	return err
}

// ProcessInflightMessage records a new JetStream message inflight awaiting ACK
func (c *jetStreamInflightMsgProcessorImpl) ProcessInflightMessage(msg *nats.Msg) error {
	// Store the message based on per-consumer sequence number of the JetStream message
	meta, err := msg.Metadata()
	if err != nil {
		log.WithError(err).WithFields(c.LogTags).Errorf("Unable to record %s", msgToString(msg))
		return err
	}
	// Sanity check the consumer name match
	if c.consumer != meta.Consumer {
		err := fmt.Errorf(
			"message expected for %s, but meta says %s", c.consumer, meta.Consumer,
		)
		log.WithError(err).WithFields(c.LogTags).Errorf("Unable to record %s", msgToString(msg))
		return err
	}

	// Fetch the per stream records
	perStreamRecords, ok := c.inflightPerStream[meta.Stream]
	if !ok {
		c.inflightPerStream[meta.Stream] = &perStreamInflightMessages{
			consumers: make(map[string]*perConsumerInflightMessages),
		}
		perStreamRecords = c.inflightPerStream[meta.Stream]
	}
	// Fetch the per consumer records
	perConsumerRecords, ok := perStreamRecords.consumers[c.consumer]
	if !ok {
		perStreamRecords.consumers[c.consumer] = &perConsumerInflightMessages{
			inflight: make(map[uint64]*nats.Msg),
		}
		perConsumerRecords = perStreamRecords.consumers[c.consumer]
	}

	perConsumerRecords.inflight[meta.Sequence.Stream] = msg
	log.WithFields(c.LogTags).Debugf("Recorded %s", msgToString(msg))
	return nil
}

// =========================================================================

type jsInflightCtrlRecordACK struct {
	timestamp time.Time
	blocking  bool
	ack       AckIndication
	resultCB  func(err error)
}

// HandlerMsgACK processes a new message ACK
func (c *jetStreamInflightMsgProcessorImpl) HandlerMsgACK(
	ack AckIndication, blocking bool, callCtxt context.Context,
) error {
	resultChan := make(chan error)
	handler := func(err error) {
		resultChan <- err
	}

	request := jsInflightCtrlRecordACK{
		timestamp: time.Now(),
		blocking:  blocking,
		ack:       ack,
		resultCB:  handler,
	}

	if err := c.tp.Submit(request, callCtxt); err != nil {
		log.WithError(err).WithFields(c.LogTags).Errorf("Failed to submit %s", ack.String())
		return err
	}

	// Don't wait for a response
	if !blocking {
		return nil
	}

	var err error
	// Wait for the response or timeout
	select {
	case result, ok := <-resultChan:
		if !ok {
			err = fmt.Errorf("response to msg ACK is invalid")
		} else {
			err = result
		}
	case <-callCtxt.Done():
		err = callCtxt.Err()
	}

	if err != nil {
		log.WithError(err).WithFields(c.LogTags).Errorf("Processing %s failed", ack.String())
	}
	return err
}

// processMsgACK support TaskProcessor, handle jsInflightCtrlRecordACK
func (c *jetStreamInflightMsgProcessorImpl) processMsgACK(param interface{}) error {
	request, ok := param.(jsInflightCtrlRecordACK)
	if !ok {
		return fmt.Errorf(
			"can not process unknown type %s for handle new ACK message",
			reflect.TypeOf(param),
		)
	}
	err := c.ProcessMsgACK(request.ack)
	if request.blocking {
		request.resultCB(err)
	}
	return err
}

// ProcessMsgACK processes a new message ACK
func (c *jetStreamInflightMsgProcessorImpl) ProcessMsgACK(ack AckIndication) error {
	// Fetch the per stream records
	perStreamRecords, ok := c.inflightPerStream[ack.Stream]
	if !ok {
		err := fmt.Errorf("no records related to stream %s", ack.Stream)
		log.WithError(err).WithFields(c.LogTags).Errorf("Unable to process %s", ack.String())
		return err
	}
	// Fetch the per consumer records
	perConsumerRecords, ok := perStreamRecords.consumers[ack.Consumer]
	if !ok {
		err := fmt.Errorf("no records related to consumer %s on stream %s", ack.Consumer, ack.Stream)
		log.WithError(err).WithFields(c.LogTags).Errorf("Unable to process %s", ack.String())
		return err
	}

	// ACK the stored message
	msg, ok := perConsumerRecords.inflight[ack.SeqNum.Stream]
	if !ok {
		err := fmt.Errorf(
			"no records related message [%d] for %s@%s", ack.SeqNum.Stream, ack.Consumer, ack.Stream,
		)
		log.WithError(err).WithFields(c.LogTags).Errorf("Unable to process %s", ack.String())
		return err
	}
	if err := msg.AckSync(); err != nil {
		log.WithError(err).WithFields(c.LogTags).Errorf("Unable to process %s", ack.String())
		return err
	}
	delete(perConsumerRecords.inflight, ack.SeqNum.Stream)
	log.WithFields(c.LogTags).Debugf("Cleaned up based on %s", ack.String())
	return nil
}
