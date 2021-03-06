package main

import (
	"encoding/json"
	datastructures "github.com/bbernhard/imagemonkey-playground/datastructures"
	log "github.com/sirupsen/logrus"
	"github.com/getsentry/raven-go"
	"os"
)

// Job holds the attributes needed to perform unit of work.
type Job struct {
	PredictionRequest datastructures.PredictionRequest
}

// NewWorker creates takes a numeric id and a channel w/ worker pool.
func NewWorker(id int, workerPool chan chan Job, modelDir string) Worker {
	return Worker{
		id:         id,
		jobQueue:   make(chan Job),
		workerPool: workerPool,
		quitChan:   make(chan bool),
		modelDir:   modelDir,
	}
}

type Worker struct {
	id         int
	jobQueue   chan Job
	workerPool chan chan Job
	quitChan   chan bool
	modelDir   string
}

func (w Worker) start() {
	log.Debug("[Worker] Worker ", w.id, " starting")
	predictor := NewTensorflowPredictor()
	predictor.Load(w.modelDir)

	go func() {
		for {
			// Add my jobQueue to the worker pool.
			w.workerPool <- w.jobQueue

			select {
			case job := <-w.jobQueue:
				// Dispatcher has added a job to my jobQueue.
				tfResult, err := predictor.Predict(job.PredictionRequest.Filename)
				if err == nil {
					redisConn := redisPool.Get()

					var predictionResult datastructures.PredictionResult
					predictionResult.Uuid = job.PredictionRequest.Uuid
					predictionResult.Result = tfResult
					predictionResult.ModelInfo = predictor.modelInfo

					serialized, err := json.Marshal(predictionResult)
					if err != nil {
						log.Error("[Worker] Couldn't marshal prediction result: ", err.Error())
						raven.CaptureError(err, nil)
					} else {
						//store result with an expiration time of 1hr...it doesn't make sense to store it longer
						//than that.
						_, err = redisConn.Do("SETEX", ("predict" + job.PredictionRequest.Uuid), 3600, serialized)
						if err != nil {
							log.Error("[Worker] Couldn't set marshal result: ", err.Error())
							raven.CaptureError(err, nil)
						} else { //successfully predicted, remove file
							err = os.Remove(job.PredictionRequest.Filename)
							if err != nil {
								log.Error("[Worker] Couldn't remove file ", err.Error())
								raven.CaptureError(err, nil)
							}
						}
					}
				} else {
					log.Error("[Worker] Couln't predict: ", err.Error())
					raven.CaptureError(err, nil)
				}

			case <-w.quitChan:
				// We have been asked to stop.
				log.Debug("[Worker] Worker ", w.id, " stopping")
				return
			}
		}
	}()
}

func (w Worker) stop() {
	go func() {
		w.quitChan <- true
	}()
}

// NewDispatcher creates, and returns a new Dispatcher object.
func NewDispatcher(jobQueue chan Job, maxWorkers int, modelDir string) *Dispatcher {
	workerPool := make(chan chan Job, maxWorkers)

	return &Dispatcher{
		jobQueue:   jobQueue,
		maxWorkers: maxWorkers,
		workerPool: workerPool,
		modelDir:   modelDir,
	}
}

type Dispatcher struct {
	workerPool chan chan Job
	maxWorkers int
	jobQueue   chan Job
	modelDir   string
}

func (d *Dispatcher) run() {
	for i := 0; i < d.maxWorkers; i++ {
		worker := NewWorker(i+1, d.workerPool, d.modelDir)
		worker.start()
	}

	go d.dispatch()
}

func (d *Dispatcher) dispatch() {
	for {
		select {
		case job := <-d.jobQueue:
			go func() {
				//fmt.Printf("fetching workerJobQueue for: %s\n", job.id)
				workerJobQueue := <-d.workerPool
				//fmt.Printf("adding %s to workerJobQueue\n", job.Name)
				workerJobQueue <- job
			}()
		}
	}
}
