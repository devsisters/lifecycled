package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/sqs"
)

const (
	heartbeatFrequency = time.Second * 10
)

type Envelope struct {
	Type    string    `json:"Type"`
	Subject string    `json:"Subject"`
	Time    time.Time `json:"Time"`
	Message string    `json:"Message"`
}

type AutoscalingMessage struct {
	Time        time.Time `json:"Time"`
	GroupName   string    `json:"AutoScalingGroupName"`
	InstanceID  string    `json:"EC2InstanceId"`
	ActionToken string    `json:"LifecycleActionToken"`
	Transition  string    `json:"LifecycleTransition"`
	HookName    string    `json:"LifecycleHookName"`
}

type Daemon struct {
	InstanceID  string
	Queue       *Queue
	AutoScaling *autoscaling.AutoScaling
	Handler     *os.File
	Signals     chan os.Signal
}

func (d *Daemon) Start() error {
	ch := make(chan *sqs.Message)
	go func() {
		for m := range ch {
			var env Envelope
			var msg AutoscalingMessage

			// unmarshal outer layer
			if err := json.Unmarshal([]byte(*m.Body), &env); err != nil {
				log.WithError(err).Info("Failed to unmarshal envelope")
				continue
			}

			log.WithFields(log.Fields{
				"type":    env.Type,
				"subject": env.Subject,
			}).Debugf("Received an SQS message")

			// unmarshal inner layer
			if err := json.Unmarshal([]byte(env.Message), &msg); err != nil {
				log.WithError(err).Info("Failed to unmarshal autoscaling message")
				continue
			}

			if msg.InstanceID != d.InstanceID {
				log.WithFields(log.Fields{
					"was":    msg.InstanceID,
					"wanted": d.InstanceID,
				}).Debugf("Skipping autoscaling event, doesn't match instance id")
				continue
			}

			d.handleMessage(msg)
		}
	}()

	spotTerminations := pollSpotTermination()
	go func() {
		for notice := range spotTerminations {
			log.Infof("Got a spot instance termination notice: %v", notice)

			log.Info("Executing handler")
			timer := time.Now()
			err := executeHandler(d.Handler, []string{terminationTransition, d.InstanceID}, d.Signals)
			executeCtx := log.WithFields(log.Fields{
				"duration": time.Now().Sub(timer),
			})

			if err != nil {
				executeCtx.WithError(err).Error("Handler script failed")
				return
			}

			executeCtx.Info("Handler finished successfully")

		}
	}()

	log.Info("Listening for lifecycle notifications")
	return d.Queue.Receive(ch)
}

func (d *Daemon) handleMessage(m AutoscalingMessage) {
	ctx := log.WithFields(log.Fields{
		"transition": m.Transition,
		"instanceid": m.InstanceID,
	})

	hbt := time.NewTicker(heartbeatFrequency)
	go func() {
		for range hbt.C {
			ctx.Debug("Sending heartbeat")
			if err := sendHeartbeat(d.AutoScaling, m); err != nil {
				ctx.WithError(err).Error("Heartbeat failed")
			}
		}
	}()

	handlerCtx := log.WithFields(log.Fields{
		"transition": m.Transition,
		"instanceid": m.InstanceID,
		"handler":    d.Handler.Name(),
	})

	handlerCtx.Info("Executing handler")
	timer := time.Now()

	err := executeHandler(d.Handler, []string{m.Transition, m.InstanceID}, d.Signals)
	executeCtx := handlerCtx.WithFields(log.Fields{
		"duration": time.Now().Sub(timer),
	})
	hbt.Stop()

	if err != nil {
		executeCtx.WithError(err).Error("Handler script failed")
		return
	}

	executeCtx.Info("Handler finished successfully")

	if err = completeLifecycle(d.AutoScaling, m); err != nil {
		ctx.WithError(err).Error("Failed to complete lifecycle action")
		return
	}

	ctx.Info("Lifecycle action completed successfully")
}

func executeHandler(command *os.File, args []string, sigs chan os.Signal) error {
	cmd := exec.Command(command.Name(), args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	go func() {
		sig := <-sigs
		if cmd.Process != nil {
			cmd.Process.Signal(sig)
		}
	}()

	return cmd.Run()
}

func sendHeartbeat(svc *autoscaling.AutoScaling, m AutoscalingMessage) error {
	_, err := svc.RecordLifecycleActionHeartbeat(&autoscaling.RecordLifecycleActionHeartbeatInput{
		AutoScalingGroupName: aws.String(m.GroupName),
		LifecycleHookName:    aws.String(m.HookName),
		InstanceId:           aws.String(m.InstanceID),
		LifecycleActionToken: aws.String(m.ActionToken),
	})
	if err != nil {
		return err
	}
	return nil
}

func completeLifecycle(svc *autoscaling.AutoScaling, m AutoscalingMessage) error {
	_, err := svc.CompleteLifecycleAction(&autoscaling.CompleteLifecycleActionInput{
		AutoScalingGroupName:  aws.String(m.GroupName),
		LifecycleHookName:     aws.String(m.HookName),
		InstanceId:            aws.String(m.InstanceID),
		LifecycleActionToken:  aws.String(m.ActionToken),
		LifecycleActionResult: aws.String("CONTINUE"),
	})
	if err != nil {
		return err
	}
	return nil
}
