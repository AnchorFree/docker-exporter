package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// command line arguments
	listen   = flag.String("listen", ":8080", "Address to listen on")
	interval = flag.Duration("interval", 1*time.Minute, "Interval between docker scrapes")

	// prometheus metrics
	restartCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_restart_count",
			Help: "Current amount of restarts.",
		},
		[]string{"name", "image_id"},
	)
	containerHealthStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_healthy",
			Help: "Healthy and running if 1, and 0 if anything else",
		},
		[]string{"name", "image_id"},
	)
)

func init() {
	// Metrics have to be registered to be exposed:
	prometheus.MustRegister(restartCount)
	prometheus.MustRegister(containerHealthStatus)
}

func main() {
	flag.Parse()

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(*listen, nil))
	}()

	// TODO: Move to dedicated scrape function
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	tick := time.Tick(*interval)
	for {
		containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{All: true})
		if err != nil {
			panic(err)
		}

		for _, container := range containers {
			inspect, _ := cli.ContainerInspect(context.Background(), container.ID)

			restartCount.With(prometheus.Labels{"name": inspect.Name, "image_id": inspect.Image}).Set(float64(inspect.RestartCount))

			if inspect.State.Health != nil {
				if inspect.State.Health.Status == types.Healthy ||
					inspect.State.Health.Status == types.NoHealthcheck {
					// we treat NoHealthcheck as healthy container, if we got here
					containerHealthStatus.With(prometheus.Labels{"name": inspect.Name, "image_id": inspect.Image}).Set(1)
				} else {
					containerHealthStatus.With(prometheus.Labels{"name": inspect.Name, "image_id": inspect.Image}).Set(0)
				}
			} else {
				if inspect.State.Running {
					// running container considered as healthy, the rest are not
					containerHealthStatus.With(prometheus.Labels{"name": inspect.Name, "image_id": inspect.Image}).Set(1)
				} else {
					containerHealthStatus.With(prometheus.Labels{"name": inspect.Name, "image_id": inspect.Image}).Set(0)
				}
			}
		}

		<-tick
	}

}
