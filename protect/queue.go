package protect

import (
	"errors"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/aerokube/selenoid/jsonerror"
	"github.com/aerokube/util"
)

// Queue - struct to hold a number of sessions
type Queue struct {
	disabled bool
	limit    chan struct{}
	queued   chan struct{}
	pending  chan struct{}
	used     chan struct{}
}

func (q *Queue) Protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenAcquired := false
		select {
		case q.limit <- struct{}{}:
			tokenAcquired = true
		default:
			tokenAcquired = false
		}

		if ! tokenAcquired {
			_, noWait := r.Header["X-Selenoid-No-Wait"]
			if noWait {
				err := errors.New(http.StatusText(http.StatusTooManyRequests))
				jsonerror.TooManyRequests(err).Encode(w)
				return
			}

			if q.disabled {
				user, remote := util.RequestInfo(r)
				log.Printf("[-] [QUEUE_IS_FULL] [%s] [%s]", user, remote)
				err := errors.New("Queue Is Full")
				jsonerror.TooManyRequests(err).Encode(w)
				return
			}
		}

		user, remote := util.RequestInfo(r)
		log.Printf("[-] [NEW_REQUEST] [%s] [%s]", user, remote)
		s := time.Now()
		go func() {
			q.queued <- struct{}{}
		}()
		if ! tokenAcquired {
			select {
			case <-r.Context().Done():
				<-q.queued
				log.Printf("[-] [CLIENT_DISCONNECTED] [%s] [%s] [%s]", user, remote, time.Since(s))
				return
			case q.limit <- struct{}{}:
				// Do nothing
			}
		}
		q.pending <- struct{}{}
		<-q.queued
		log.Printf("[-] [NEW_REQUEST_ACCEPTED] [%s] [%s]", user, remote)
		next.ServeHTTP(w, r)
	}
}

// Used - get created sessions
func (q *Queue) Used() int {
	return len(q.used)
}

// Pending - get pending sessions
func (q *Queue) Pending() int {
	return len(q.pending)
}

// Queued - get queued sessions
func (q *Queue) Queued() int {
	return len(q.queued)
}

// Drop - session is not created
func (q *Queue) Drop() {
	<-q.limit
	<-q.pending
}

// Create - session is created
func (q *Queue) Create() {
	q.used <- <-q.pending
}

// Release - session is closed
func (q *Queue) Release() {
	<-q.limit
	<-q.used
}

// New - create and initialize queue
func New(size int, disabled bool) *Queue {
	return &Queue{
		disabled,
		make(chan struct{}, size),
		make(chan struct{}, math.MaxInt32),
		make(chan struct{}, math.MaxInt32),
		make(chan struct{}, math.MaxInt32),
	}
}
