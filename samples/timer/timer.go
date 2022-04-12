package main

import (
	"context"
	"log"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/backend/redis"
	"github.com/cschleiden/go-workflows/backend/sqlite"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"
	"github.com/google/uuid"
)

func main() {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	b := sqlite.NewInMemoryBackend()
	b, err := redis.NewRedisBackend("localhost:6379", "", "RedisPassw0rd", 0)
	if err != nil {
		panic(err)
	}

	// Run worker
	w := RunWorker(ctx, b)

	// Start workflow via client
	c := client.New(b)

	startWorkflow(ctx, c)

	cancel()

	w.Stop()
}

func startWorkflow(ctx context.Context, c client.Client) {
	wf, err := c.CreateWorkflowInstance(ctx, client.WorkflowInstanceOptions{
		InstanceID: uuid.NewString(),
	}, Workflow1, "Hello world")
	if err != nil {
		panic("could not start workflow")
	}

	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond * 200)
		c.SignalWorkflow(ctx, wf.InstanceID, "Hello world", "value")
	}

	result, err := client.GetWorkflowResult[string](ctx, c, wf, time.Second*10)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Workflow finished. Result:", result)
}

func RunWorker(ctx context.Context, mb backend.Backend) worker.Worker {
	w := worker.New(mb, nil)

	w.RegisterWorkflow(Workflow1)

	w.RegisterActivity(Activity1)

	if err := w.Start(ctx); err != nil {
		panic("could not start worker")
	}

	return w
}

func Workflow1(ctx workflow.Context, msg string) (string, error) {
	logger := workflow.Logger(ctx)
	logger.Debug("Entering Workflow1, input: ", msg)
	defer logger.Debug("Leaving Workflow1")

	a1 := workflow.ExecuteActivity[int](ctx, workflow.DefaultActivityOptions, Activity1, 35, 12)

	tctx, _ := workflow.WithCancel(ctx)
	// cancel()

	workflow.Select(
		ctx,
		workflow.Await(workflow.ScheduleTimer(tctx, 2*time.Second), func(ctx workflow.Context, f workflow.Future[struct{}]) {
			if _, err := f.Get(ctx); err != nil {
				logger.Debug("Timer canceled")
			} else {
				logger.Debug("Timer fired")
			}
		}),
		workflow.Await(a1, func(ctx workflow.Context, f workflow.Future[int]) {
			r, err := f.Get(ctx)
			if err != nil {
				panic(err)
			}

			logger.Debug("Activity result", r)

			// Cancel timer
			// cancel()
		}),
	)

	return "result", nil
}

func Activity1(ctx context.Context, a, b int) (int, error) {
	log.Println("Entering Activity1")

	time.Sleep(10 * time.Second)

	defer func() {
		log.Println("Leaving Activity1")
	}()

	return a + b, nil
}
