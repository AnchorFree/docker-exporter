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

	// maps container id -> closer channel
	scrapers = make(map[string]chan bool)

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
	inspectTimeoutStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_container_stuck_inspect",
			Help: "Inspect worked if 1, and 0 if timed out",
		},
		[]string{"name", "image_id"},
	)
)

func init() {
	// Metrics have to be registered to be exposed:
	prometheus.MustRegister(restartCount)
	prometheus.MustRegister(containerHealthStatus)
	prometheus.MustRegister(inspectTimeoutStatus)
}

func scrapeContainer(container types.Container, cli *client.Client, closer <-chan bool) {
	tick := time.Tick(*interval)
	timeout := time.Duration(interval.Nanoseconds() * 3 / 4) // 3/4 of the interval seems like a reasonable timeout
	timeoutContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	inspect, err := cli.ContainerInspect(context.Background(), container.ID)
	if err != nil {
		log.Printf("first inspect failed for container %s error: %s", container.ID, err)
		return
	}
	name := inspect.Name
	labels := prometheus.Labels{"name": name, "image_id": inspect.Image}
	//log.Printf("starting scraper for %s", name)

	for {
		select {
		case <-closer:
			//log.Printf("stopping scraper for %s", name)
			closer = nil
			return
		case <-tick:
			inspect, err := cli.ContainerInspect(timeoutContext, container.ID)
			if err != nil {
				if err == context.DeadlineExceeded {
					inspectTimeoutStatus.With(labels).Set(0)
					continue
				} else {
					inspectTimeoutStatus.With(labels).Set(1)
				}
			}

			restartCount.With(labels).Set(float64(inspect.RestartCount))

			if inspect.State.Health != nil {
				if inspect.State.Health.Status == types.Healthy ||
					inspect.State.Health.Status == types.NoHealthcheck {
					// we treat NoHealthcheck as healthy container, if we got here
					containerHealthStatus.With(labels).Set(1)
				} else {
					containerHealthStatus.With(labels).Set(0)
				}
			} else {
				if inspect.State.Running {
					// running container considered as healthy, the rest are not
					containerHealthStatus.With(labels).Set(1)
				} else {
					containerHealthStatus.With(labels).Set(0)
				}
			}
		}
	}
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

		newScrapers := make(map[string]chan bool)
		for _, container := range containers {
			if _, present := scrapers[container.ID]; present {
				newScrapers[container.ID] = scrapers[container.ID]
			} else {
				//log.Printf("new container: %s", container.ID)
				newScrapers[container.ID] = make(chan bool)
				go scrapeContainer(container, cli, newScrapers[container.ID])
			}
		}

		// detect containers which have gone away and kill their scrapers
		for containerId, closer := range scrapers {
			if _, present := newScrapers[containerId]; !present {
				//log.Printf("container gone: %s", containerId)
				closer <- true
				close(closer)
			}
		}

		// and swap
		scrapers = newScrapers

		<-tick
	}

}
