package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Status is the service status
type Status string

const (
	// UP represents a service that is running (with at least a container running)
	UP Status = "running"
	// DOWN represents a service that is not running (with 0 container running)
	DOWN Status = "down"
	// STARTING represents a service that is starting (with at least a container starting)
	STARTING Status = "starting"
	// UNKNOWN represents a service for which the docker status is not know
	UNKNOWN Status = "unknown"
)

// Service holds all information related to a service
type Service struct {
	name      string
	timeout   uint64
	host      string
	path      string
	time      chan uint64
	mu        sync.Mutex
	isHandled bool
}

// claimHandler reports whether the caller is the goroutine responsible for
// stopping the service, so only one stopAfterTimeout runs per service.
func (service *Service) claimHandler() bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.isHandled {
		return false
	}
	service.isHandled = true
	return true
}

// releaseHandler hands responsibility back once the service has been stopped,
// so a later request can start it again.
func (service *Service) releaseHandler() {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.isHandled = false
}

var (
	servicesMu sync.Mutex
	services   = map[string]*Service{}
)

func main() {
	fmt.Println("Server listening on port 10000.")
	http.HandleFunc("/api/", handleRequests())
	log.Fatal(http.ListenAndServe(":10000", nil))
}

func handleRequests() func(w http.ResponseWriter, r *http.Request) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatal(fmt.Errorf("%+v", "Could not connect to docker API"))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		serviceName, serviceTimeout, serviceHost, servicePath, err := parseParams(r)
		fmt.Println(serviceName, serviceTimeout)
		if err != nil || serviceName == "" || serviceTimeout == 0 {
			fmt.Fprintf(w, "error: %+v, service name = `%s`, timeout = `%d`", err, serviceName, serviceTimeout)
			return
		}
		service, err := GetOrCreateService(serviceName, serviceTimeout, serviceHost, servicePath, cli)
		if err != nil {
			fmt.Printf("Error: %+v\n ", err)
			fmt.Fprintf(w, "%+v", err)
			return
		}
		status, err := service.HandleServiceState(cli)
		if err != nil {
			fmt.Printf("Error: %+v\n ", err)
			fmt.Fprintf(w, "%+v", err)
		}
		fmt.Fprintf(w, "%+s", status)
	}
}

func getParam(queryParams url.Values, paramName string) (string, error) {
	if queryParams[paramName] == nil {
		return "", fmt.Errorf("%s is required", paramName)
	}
	return queryParams[paramName][0], nil
}

func parseParams(r *http.Request) (string, uint64, string, string, error) {
	queryParams := r.URL.Query()

	serviceName, err := getParam(queryParams, "name")
	if err != nil {
		return "", 0, "", "", nil
	}

	host, _ := getParam(queryParams, "host")
	serviceHost, err := url.QueryUnescape(host)
	if err != nil {
		return "", 0, "", "", nil
	}

	path, _ := getParam(queryParams, "path")
	servicePath, err := url.QueryUnescape(path)
	if err != nil {
		return "", 0, "", "", nil
	}

	timeoutString, err := getParam(queryParams, "timeout")
	if err != nil {
		return "", 0, "", "", nil
	}
	serviceTimeout, err := strconv.Atoi(timeoutString)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("timeout should be an integer")
	}
	return serviceName, uint64(serviceTimeout), serviceHost, servicePath, nil
}

func GetOrCreateService(name string, timeout uint64, host, path string, client *client.Client) (*Service, error) {
	if name == "generic-container-manager" {
		checkerService := Service{
			name:    name,
			timeout: timeout,
			host:    host,
			path:    path,
			time:    make(chan uint64, 1),
		}
		ctx := context.Background()
		containers, err := checkerService.getDockerContainers(ctx, client)
		if err != nil {
			return nil, err
		}
		name = containers[0].Labels["traefik-container-manager.name"]
	}

	servicesMu.Lock()
	defer servicesMu.Unlock()
	if service, ok := services[name]; ok {
		return service, nil
	}
	service := &Service{
		name:    name,
		timeout: timeout,
		host:    host,
		path:    path,
		time:    make(chan uint64, 1),
	}

	services[name] = service
	return service, nil
}

// HandleServiceState up the service if down or set timeout for downing the service
func (service *Service) HandleServiceState(cli *client.Client) (string, error) {
	status, err := service.getStatus(cli)
	// return "", fmt.Errorf("state: %s, %+v", status, err)
	if err != nil {
		return "", err
	}
	if status == UP {
		fmt.Printf("- Service %v is up\n", service.name)
		if service.claimHandler() {
			go service.stopAfterTimeout(cli)
		}
		select {
		case service.time <- service.timeout:
			fmt.Println("Sent delay")
		default:
		}
		return "started", nil
	} else if status == STARTING {
		fmt.Printf("- Service %v is starting\n", service.name)
		if service.claimHandler() {
			go service.stopAfterTimeout(cli)
		}
		return "starting", nil
	} else if status == DOWN {
		fmt.Printf("- Service %v is down\n", service.name)
		service.start(cli)
		return "starting", nil
	} else {
		fmt.Printf("- Service %v status is unknown\n", service.name)
		if err != nil {
			return "", err
		}
		return service.HandleServiceState(cli)
	}
}

func (service *Service) getStatus(client *client.Client) (Status, error) {
	ctx := context.Background()
	var status Status = UNKNOWN
	containers, err := service.getDockerContainers(ctx, client)
	if err != nil {
		return status, err
	}
	for _, container := range containers {
		switch container.State {
		case "running":
		default:
			status = DOWN
		}
	}
	if status != DOWN {
		status = UP
	}

	return status, nil
}

func (service *Service) start(client *client.Client) {
	fmt.Printf("Starting service %s\n", service.name)
	service.startContainers(client)
	if service.claimHandler() {
		go service.stopAfterTimeout(client)
	}
	select {
	case service.time <- service.timeout:
	default:
	}
}

func (service *Service) stopAfterTimeout(client *client.Client) {
	defer service.releaseHandler()
	fmt.Println("In stopAfterTimeout")

	if timeout, ok := <-service.time; ok {
		fmt.Println("Sleeping", timeout)
		time.Sleep(time.Duration(timeout) * time.Second)
	} else {
		fmt.Println("That should not happen, but we never know ;)")
	}

	for {
		select {
		case timeout, ok := <-service.time:
			if ok {
				fmt.Println("Sleeping", timeout)
				time.Sleep(time.Duration(timeout) * time.Second)
			} else {
				fmt.Println("That should not happen, but we never know ;)")
			}
		default:
			fmt.Printf("Stopping service %s\n", service.name)
			service.stopContainers(client)
			return
		}
	}
}

func (service *Service) startContainers(client *client.Client) error {
	ctx := context.Background()
	containers, err := service.getDockerContainers(ctx, client)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.State != "running" {
			if err := client.ContainerStart(ctx, container.ID, types.ContainerStartOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (service *Service) stopContainers(client *client.Client) error {
	ctx := context.Background()
	containers, err := service.getDockerContainers(ctx, client)
	if err != nil {
		return err
	}
	for _, container := range containers {
		fmt.Println(container.Image, container.State)
		if container.State == "running" {
			if err := client.ContainerStop(ctx, container.ID, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func prefixMatch(requested, label string) bool {
	if requested == "" || label == "" {
		return false
	}
	return strings.HasPrefix(requested, label) || strings.HasPrefix(label, requested)
}

func (service *Service) matches(container types.Container) bool {
	if service.name != "" && strings.EqualFold(service.name, container.Labels["traefik-container-manager.name"]) {
		return true
	}
	if prefixMatch(service.host, container.Labels["traefik-container-manager.host"]) {
		return true
	}
	return prefixMatch(service.path, container.Labels["traefik-container-manager.path"])
}

func (service *Service) getDockerContainers(ctx context.Context, client *client.Client) ([]types.Container, error) {
	opts := types.ContainerListOptions{All: true}
	opts.Filters = filters.NewArgs()
	opts.Filters.Add("label", "traefik-container-manager.name")
	containers, err := client.ContainerList(ctx, opts)
	if err != nil {
		return nil, err
	}
	requiredContainers := make([]types.Container, 0)
	for _, container := range containers {
		if service.matches(container) {
			requiredContainers = append(requiredContainers, container)
		}
	}
	if len(requiredContainers) == 0 {
		return requiredContainers, fmt.Errorf("no containers found")
	}
	return requiredContainers, nil
}
