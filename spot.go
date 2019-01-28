package main

import (
	"io/ioutil"
	"net/http"
	"time"

	log "github.com/Sirupsen/logrus"
)

const (
	metadataURLTerminationTime = "http://169.254.169.254/latest/meta-data/spot/termination-time"
	terminationTransition      = "ec2:SPOT_INSTANCE_TERMINATION"
	terminationTimeFormat      = "2006-01-02T15:04:05Z"
)

func pollSpotTermination() chan time.Time {
	ch := make(chan time.Time)

	log.Debugf("Polling metadata service for spot termination notices")
	go func() {
		for range time.NewTicker(time.Second * 5).C {
			// 이렇게 함수로 한 번 더 감싸지 않으면 이 함수가 끝나지 않아 아래 res.Body.Close()가
			// 불리지않는다. 메모리릭으로 이어지기 때문에 방어코드를
			// 짰음
			func() {
				res, err := http.Get(metadataURLTerminationTime)
				if err != nil {
					log.WithError(err).Info("Failed to query metadata service")
					return
				}
				defer res.Body.Close()

				// will return 200 OK with termination notice
				if res.StatusCode != http.StatusOK {
					return
				}

				body, err := ioutil.ReadAll(res.Body)
				if err != nil {
					log.WithError(err).Info("Failed to read response from metadata service")
					return
				}

				// if 200 OK, expect a body like 2015-01-05T18:02:00Z
				t, err := time.Parse(terminationTimeFormat, string(body))
				if err != nil {
					log.WithError(err).Info("Failed to parse time in termination notice")
					return
				}

				ch <- t
			}()
		}
	}()

	return ch
}
