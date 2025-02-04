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

// ForwardMessageHandlerCB callback used to forward new messages to the next pipeline stage
type ForwardMessageHandlerCB func(msg *nats.Msg, ctxt context.Context) error

// AlertOnErrorCB callback used to expose internal error to an outer context for handling
type AlertOnErrorCB func(err error)

// JetStreamPushSubscriber is directly reading from JetStream with a push consumer
type JetStreamPushSubscriber interface {
	// StartReading begin reading data from JetStream
	StartReading(
		forwardCB ForwardMessageHandlerCB,
		errorCB AlertOnErrorCB,
		wg *sync.WaitGroup,
		ctxt context.Context,
	) error
}

// jetStreamPushSubscriberImpl implements JetStreamPushSubscriber
type jetStreamPushSubscriberImpl struct {
	common.Component
	nats       *core.NatsClient
	reading    bool
	sub        *nats.Subscription
	forwardMsg ForwardMessageHandlerCB
	errorCB    AlertOnErrorCB
	lock       *sync.Mutex
}

// getJetStreamPushSubscriber define new JetStreamPushSubscriber
func getJetStreamPushSubscriber(
	natsClient *core.NatsClient, stream, subject, consumer string, deliveryGroup *string,
) (JetStreamPushSubscriber, error) {
	logTags := log.Fields{
		"module":    "dataplane",
		"component": "js-push-reader",
		"stream":    stream,
		"subject":   subject,
		"consumer":  consumer,
	}
	// Create the subscription now
	var s *nats.Subscription
	var err error
	// Build the subscription based on whether deliveryGroup is defined
	if deliveryGroup != nil {
		s, err = natsClient.JetStream().QueueSubscribeSync(
			subject, *deliveryGroup, nats.Durable(consumer),
		)
	} else {
		s, err = natsClient.JetStream().SubscribeSync(subject, nats.Durable(consumer))
	}
	if err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to define subscription")
		return nil, err
	}
	return &jetStreamPushSubscriberImpl{
		Component:  common.Component{LogTags: logTags},
		nats:       natsClient,
		sub:        s,
		forwardMsg: nil,
		errorCB:    nil,
		lock:       &sync.Mutex{},
	}, nil
}

// StartReading begin reading data from JetStream
func (r *jetStreamPushSubscriberImpl) StartReading(
	forwardCB ForwardMessageHandlerCB,
	errorCB AlertOnErrorCB,
	wg *sync.WaitGroup,
	ctxt context.Context,
) error {
	localLogTags, err := common.UpdateLogTags(r.LogTags, ctxt)
	if err != nil {
		log.WithError(err).WithFields(r.LogTags).Errorf("Failed to update logtags")
		return err
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	// Already reading
	if r.reading {
		err := fmt.Errorf("already reading")
		log.WithError(err).WithFields(localLogTags).Error("Unable to start reading")
		return err
	}
	wg.Add(1)
	r.forwardMsg = forwardCB
	r.errorCB = errorCB
	r.reading = true
	// Start reading from JetStream
	go func() {
		defer wg.Done()
		log.WithFields(localLogTags).Infof("Starting reading from JetStream")
		defer log.WithFields(localLogTags).Infof("Stopping JetStream read loop")
		defer func() {
			if err := r.sub.Unsubscribe(); err != nil {
				log.WithError(err).WithFields(localLogTags).Error("Unsubscribe failed")
			} else {
				log.WithFields(localLogTags).Infof("Unsubscribed from subject")
			}
		}()
		defer func() {
			if err := r.sub.Drain(); err != nil {
				log.WithError(err).WithFields(localLogTags).Error("Drain failed")
			} else {
				log.WithFields(localLogTags).Infof("Drained subscription")
			}
		}()
		for {
			newMsg, err := r.sub.NextMsgWithContext(ctxt)
			if err != nil {
				log.WithError(err).WithFields(localLogTags).Errorf("Read failure")
				r.errorCB(err)
				break
			}
			// Forward the message
			if newMsg != nil {
				log.WithFields(localLogTags).Debugf("Received %s", msgToString(newMsg))
				if err := r.forwardMsg(newMsg, ctxt); err != nil {
					log.WithError(err).WithFields(localLogTags).Errorf("Unable to forward messages")
					r.errorCB(err)
				}
			}
		}
	}()
	return nil
}

// ==============================================================================

// JetStreamPublisher publishes new messages into JetStream
type JetStreamPublisher interface {
	// Publish publishes a new message into JetStream on a subject
	Publish(subject string, msg []byte, ctxt context.Context) error
}

// jetStreamPublisherImpl implements JetStreamPublisher
type jetStreamPublisherImpl struct {
	common.Component
	nats *core.NatsClient
}

// GetJetStreamPublisher get new JetStreamPublisher
func GetJetStreamPublisher(
	natsClient *core.NatsClient, instance string,
) (JetStreamPublisher, error) {
	logTags := log.Fields{
		"module": "dataplane", "component": "js-publisher", "instance": instance,
	}
	return &jetStreamPublisherImpl{
		Component: common.Component{LogTags: logTags}, nats: natsClient,
	}, nil
}

// Publish publishes a new message into JetStream on a subject
func (s *jetStreamPublisherImpl) Publish(subject string, msg []byte, ctxt context.Context) error {
	localLogTags, err := common.UpdateLogTags(s.LogTags, ctxt)
	if err != nil {
		log.WithError(err).WithFields(s.LogTags).Errorf("Failed to update logtags")
		return err
	}
	ack, err := s.nats.JetStream().PublishAsync(subject, msg)
	if err != nil {
		log.WithError(err).WithFields(localLogTags).Errorf("Unable to send message")
		return err
	}
	// Wait for success, failure, or timeout
	select {
	case goodSig, ok := <-ack.Ok():
		if !ok {
			err := fmt.Errorf("reading nats.PubAckFuture OK channel failure")
			log.WithError(err).WithFields(localLogTags).Errorf("Message send failure")
			return err
		}
		log.WithFields(localLogTags).Debugf(
			"Sent [%d] to %s/%s", goodSig.Sequence, goodSig.Stream, subject,
		)
		return nil
	case txErr, ok := <-ack.Err():
		if !ok {
			err := fmt.Errorf("reading nats.PubAckFuture error channel failure")
			log.WithError(err).WithFields(localLogTags).Errorf("Message send failure")
			return err
		}
		return txErr
	case <-ctxt.Done():
		err := ctxt.Err()
		log.WithError(err).WithFields(localLogTags).Errorf("Message send timed out")
		return err
	}
}
