package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	logger *zap.Logger
	wg     *sync.WaitGroup

	clientset      *kubernetes.Clientset
	namespace      = os.Getenv("INGRESSNAMESPACE")
	podIngressName = os.Getenv("INGRESSPODNAME")
	logLevel       = os.Getenv("LOGLEVEL")
	follow         = true

	connectionCounter *connectionCounterStruct
	podsWatched       = map[string]bool{}
)

type connectionCounterStruct struct {
	mutex         sync.Mutex
	connectionMap map[string]float64
}

type nginxLog struct {
	Time                string `json:"time,omitempty"`
	Proxy_protocol_addr string `json:"proxy_protocol_addr,omitempty"`
	Remote_addr         string `json:"remote_addr"`
	Xforwardfor         string `json:"x-forward-for,omitempty"`
	Request_id          string `json:"request_id,omitempty"`
	Request             string `json:"request,omitempty"`
	Remote_user         string `json:"remote_user,omitempty"`
	Bytes_sent          string `json:"bytes_sent,omitempty"`
	Body_bytes_sent     string `json:"body_bytes_sent,omitempty"`
	Request_time        string `json:"request_time,omitempty"`
	Status              string `json:"status,omitempty"`
	Vhost               string `json:"vhost,omitempty"`
	Request_proto       string `json:"request_proto,omitempty"`
	Path                string `json:"path,omitempty"`
	Request_query       string `json:"request_query,omitempty"`
	Request_length      string `json:"request_length,omitempty"`
	Method              string `json:"method,omitempty"`
	Http_referrer       string `json:"http_referrer,omitempty"`
	Http_user_agent     string `json:"http_user_agent,omitempty"`
	Upstream            string `json:"upstream,omitempty"`
	Upstream_ip         string `json:"upstream_ip,omitempty"`
	Upstream_latency    string `json:"upstream_latency,omitempty"`
	Upstream_status     string `json:"upstream_status,omitempty"`
}

func counterWriter(logs []nginxLog) {
	connectionCounter.mutex.Lock()
	for _, value := range logs {
		connectionCounter.connectionMap[value.Remote_addr]++
	}
	connectionCounter.mutex.Unlock()
}

func watchPodLogs(ctx context.Context, podName string, containerName string, logChannel chan string) {
	wg.Add(1)
	podsWatched[podName] = true
	
	defer wg.Done()
	defer logger.Sugar().Infof("Pod deleted %s/%s, remove from watch", podName, containerName)
	defer delete(podsWatched, podName)

	count := int64(0) // only new lines read
	podLogOptions := corev1.PodLogOptions{
		Container: containerName,
		Follow:    follow,
		TailLines: &count,
	}

	podLogRequest := clientset.CoreV1().Pods(namespace).GetLogs(podName, &podLogOptions)
	for {
		stream, err := podLogRequest.Stream(ctx)
		if err != nil {
			logger.Sugar().Errorf("Unable to get %s/%s log stream, due to err: %v", podName, containerName, err)
			return
		}

		reader := bufio.NewScanner(stream)
		for reader.Scan() {
			select {
			case <-ctx.Done():
				logger.Sugar().Debug("Log scanner %s/%s close, due to get ctx.Done, : %v")
				if err = stream.Close(); err != nil {
					logger.Sugar().Errorf("Log scanner %s/%s get error while podLogRequest.Stream close: %v", podName, containerName, err)
				}
				return
			default:
				line := reader.Text()
				logChannel <- line
			}
		}

		if err = reader.Err(); err != nil {
			logger.Sugar().Errorf("Log scanner %s/%s get error from bufio.NewScanner: %v", podName, containerName, err)
		}

		if err = stream.Close(); err != nil {
			logger.Sugar().Errorf("Log scanner %s/%s get error while podLogRequest.Stream close: %v", podName, containerName, err)
		}
	}
}

func isPodLogWatched(podName string) bool {
	_, ok := podsWatched[podName]
	return ok
}

func podIsReady(podConditions []corev1.PodCondition) bool {
	for _, condition := range podConditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return true
		}
	}
	return false
}

func getPods(ctx context.Context, namespace string) (*corev1.PodList, error) {
	podInterface := clientset.CoreV1().Pods(namespace)
	podList, err := podInterface.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return podList, nil
}

func podEventProcessing(ctx context.Context, event watch.Event, pod *corev1.Pod, logChannel chan string) {
	if !strings.Contains(pod.Name, podIngressName) || isPodLogWatched(pod.Name) {
		return
	}

	switch event.Type {
	case watch.Modified:
		switch pod.Status.Phase {
		case corev1.PodRunning:
			if podIsReady(pod.Status.Conditions) {
				logger.Sugar().Infof("Found new pod created: %s, add to watching logs\n", pod.Name)
				go watchPodLogs(ctx, pod.Name, pod.Spec.Containers[0].Name, logChannel)
			}
		}
	}
}

// watch for new created pods and add to logging
func watchPods(ctx context.Context, logChannel chan string) {
	wg.Add(1)
	defer wg.Done()

	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Sugar().Fatalf("cannot create Pod event watcher, due to err: %v", err)
	}

	for {
		select {
		case event := <-watcher.ResultChan():
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}

			podEventProcessing(ctx, event, pod, logChannel)
		case <-ctx.Done():
			watcher.Stop()
			return
		}
	}
}

func exposeMetrics(w http.ResponseWriter, req *http.Request) {
	var buf bytes.Buffer

	buf.WriteString("# HELP nginx_connections_by_remote_addr Client connections by remote_addr\n")
	buf.WriteString("# TYPE nginx_connections_by_remote_addr counter\n")

	connectionCounter.mutex.Lock()
	defer connectionCounter.mutex.Unlock()

	for ip, count := range connectionCounter.connectionMap {
		buf.WriteString(fmt.Sprintf("nginx_connections_by_remote_addr{remote_addr=\"%s\"} %v\n", ip, count))
	}

	_, err := fmt.Fprint(w, buf.String())
	if err != nil {
		logger.Sugar().Errorf("Can't expose metrics, due to err: %v", err)
	}
}

func stats(w http.ResponseWriter, req *http.Request) {
	response := fmt.Sprintf("goroutines %d\n", runtime.NumGoroutine())
	_, err := fmt.Fprint(w, response)
	if err != nil {
		logger.Sugar().Errorf("Can't expose statistic, due to err: %v", err)
	}
}

//web server to expose prometheus-like metrics
func startWebServer() {
	http.HandleFunc("/metrics", exposeMetrics)
	http.HandleFunc("/stats", stats)
	logger.Sugar().Fatal(http.ListenAndServe(":8080", nil))
}

func main() {
	if logLevel == "debug" {
		logger = zap.Must(zap.NewDevelopment())
	} else {
		logger = zap.Must(zap.NewProduction())
	}

	wg = &sync.WaitGroup{}
	defer wg.Wait()
	// flushes buffer before exit, if any
	defer func() {
		if err := logger.Sync(); err != nil {
			logger.Sugar().Fatalf("Error when logger sync before exit: %v", err)
		}
	}()

	connectionCounter = &connectionCounterStruct{
		mutex:         sync.Mutex{},
		connectionMap: map[string]float64{},
	}

	logChannel := make(chan string)
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)

	
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	podList, err := getPods(ctx, namespace)
	if err != nil {
		panic(err)
	}

	logger.Sugar().Infof("Search pods in namespace: \"%s\" and name should contains: \"%s\"", namespace, podIngressName)
	for _, pod := range podList.Items {
		if strings.Contains(pod.Name, podIngressName) {
			logger.Sugar().Infof("Found pod: \"%s\", add to watching logs", pod.Name)
			go watchPodLogs(ctx, pod.Name, pod.Spec.Containers[0].Name, logChannel)
		}
	}

	go watchPods(ctx, logChannel)
	go startWebServer()
	logger.Sugar().Debug("Webserver started")

	cache := []nginxLog{}
	for {
		select {
		case log := <-logChannel:
			logger.Sugar().Debugf("Get string: %s", log)
			if !strings.Contains(log, "{") {
				continue
			}

			loggedRequest := &nginxLog{}
			err = json.Unmarshal([]byte(log), loggedRequest)
			if err != nil {
				logger.Sugar().Errorf("Failed to unmarshal text, due to err: %v.\nText:%s", err, log)
			} else {
				cache = append(cache, *loggedRequest)
				if len(cache) == 100 {
					go counterWriter(cache)
					cache = nil
				}
			}
		case <-ctx.Done():
			return
		// TODO find the cause of the stuck
		case <-time.After(1 * time.Minute):
			logger.Sugar().Error("Dont get message for 1 minute, maybe stuck. Exit to restart")
			cancel()
			os.Exit(1)
		}
	}
}
