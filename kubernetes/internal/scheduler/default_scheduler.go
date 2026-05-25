// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

var _ Task = &taskNode{}

var (
	timeNow = func() time.Time {
		return time.Now()
	}
)

type taskSpec struct {
	Process         *api.Process
	PodTemplateSpec *corev1.PodTemplateSpec
}

type taskNode struct {
	metav1.ObjectMeta
	Spec taskSpec

	// status
	Status  *api.Task
	IP      string
	PodName string

	// collect from endpoints
	tState              TaskState
	tStateLastTransTime *time.Time

	// inner sch state
	sStateLastTransTime *time.Time
	sState              string
}

func (t *taskNode) GetPodName() string {
	return t.PodName
}

func (t *taskNode) GetState() TaskState {
	return t.tState
}

func (t *taskNode) IsResourceReleased() bool {
	return t.sState == stateReleased
}

func (t *taskNode) isTaskCompleted() bool {
	return t.tState == SucceedTaskState || t.tState == FailedTaskState
}

func (t *taskNode) isTaskDeleted() bool {
	return t.Status == nil
}

func (t *taskNode) transSchState(to string, log logr.Logger) {
	if t.sState == to {
		return
	}
	from := t.sState
	t.sState = to
	var lat time.Duration
	now := timeNow()
	if t.sStateLastTransTime != nil {
		lat = now.Sub(*t.sStateLastTransTime)
	}
	t.sStateLastTransTime = ptr.To[time.Time](now)
	log.Info("task node trans sch state", "name", t.Name, "namespace", t.Namespace, "from", from, "to", to, "latencyMs", lat.Milliseconds())
}

func (t *taskNode) transTaskState(to TaskState, log logr.Logger) {
	if t.tState == to {
		return
	}
	from := t.tState
	t.tState = to
	var lat time.Duration
	now := timeNow()
	if t.tStateLastTransTime != nil {
		lat = now.Sub(*t.tStateLastTransTime)
	}
	t.tStateLastTransTime = ptr.To[time.Time](now)
	log.Info("task node trans task state", "name", t.Name, "namespace", t.Namespace, "from", from, "to", to, "latencyMs", lat.Milliseconds())
}

const (
	// FSM: TaskNode Sch State Machine
	/*
	   $start --> pending

	   pending -- "when task is assigned to Pod" --> assigned
	   pending -- "when BatchSandbox's deletion timestamp != 0" --> released

	   assigned -- "when BatchSandbox's deletion timestamp != 0" --> releasing
	   assigned -- "when task state is SUCCEED && policy is allowed" --> releasing
	   assigned -- "when task state is FAILED && policy is allowed" --> releasing
	   assigned -- "set Task"

	   releasing -- "when endpoint returns nil task or endpoint lost too many times  (e.g., force-deleted), endpoint is nil(unassigned)" --> released

	   released --> $end
	*/
	//statePending   = "pending", endpoint is empty means pending, otherwise means assigned
	//stateAssigned  = "assigned"
	stateReleasing = "releasing"
	stateReleased  = "released"
	stateUnknown   = "unknown"
)

type taskClient interface {
	Set(ctx context.Context, task *api.Task) (*api.Task, error)
	Get(ctx context.Context) (*api.Task, error)
}

const (
	defaultTimeout        time.Duration = 3 * time.Second
	defaultTaskPort                     = "5758"
	defaultSchConcurrency int           = 10
)

func newTaskClient(ip string) taskClient {
	return api.NewClient(fmtEndpoint(ip))
}

func fmtEndpoint(podIP string) string {
	return fmt.Sprintf("http://%s:%s", podIP, defaultTaskPort)
}

type defaultTaskScheduler struct {
	freePods []*corev1.Pod
	allPods  []*corev1.Pod

	taskNodes           []*taskNode
	taskNodeByNameIndex map[string]*taskNode

	maxConcurrency int
	once           sync.Once

	taskStatusCollector       taskStatusCollector
	taskClientCreator         taskClientCreator
	resPolicyWhenTaskComplete sandboxv1alpha1.TaskResourcePolicy
	name                      string
	logger                    logr.Logger
}

func newTaskScheduler(name string, tasks []*api.Task, pods []*corev1.Pod, resPolicyWhenTaskComplete sandboxv1alpha1.TaskResourcePolicy, logger logr.Logger) (*defaultTaskScheduler, error) {
	sch := &defaultTaskScheduler{
		allPods:                   pods,
		maxConcurrency:            defaultSchConcurrency,
		taskClientCreator:         newTaskClient,
		taskStatusCollector:       newTaskStatusCollector(newTaskClient, logger),
		resPolicyWhenTaskComplete: resPolicyWhenTaskComplete,
		name:                      name,
		logger:                    logger,
	}
	taskNodes, err := initTaskNodes(tasks)
	if err != nil {
		return nil, fmt.Errorf("scheduler: failed to init task node err %w", err)
	}
	sch.taskNodes = taskNodes
	sch.taskNodeByNameIndex = indexByName(taskNodes)
	logger.Info("successfully init task nodes", "scheduler", name, "size", len(taskNodes))
	// TODO: Optimization – skip recovery for a brand-new scheduler.
	// Recovery is unnecessary in this case and incurs significant overhead.
	if err := sch.recover(); err != nil {
		return nil, fmt.Errorf("scheduler: failed to recover, err %w", err)
	}
	logger.Info("successfully recover", "scheduler", name)
	return sch, nil
}

func indexByName(taskNodes []*taskNode) map[string]*taskNode {
	ret := make(map[string]*taskNode, len(taskNodes))
	for i := range taskNodes {
		ret[taskNodes[i].Name] = taskNodes[i]
	}
	return ret
}

func (sch *defaultTaskScheduler) Schedule() error {
	sch.refreshFreePods()
	sch.collectTaskStatus(sch.taskNodes)
	return sch.scheduleTaskNodes()
}

func (sch *defaultTaskScheduler) UpdatePods(pods []*corev1.Pod) {
	sch.allPods = pods
}

// AddTasks registers task specs that are not yet tracked by the scheduler.
// Tasks whose names are already tracked are silently skipped, making this
// safe to call with the full task list during a scale-out reconciliation.
func (sch *defaultTaskScheduler) AddTasks(tasks []*api.Task) error {
	newNodes, err := initTaskNodes(tasks)
	if err != nil {
		return err
	}
	for _, node := range newNodes {
		if _, exists := sch.taskNodeByNameIndex[node.Name]; !exists {
			sch.taskNodes = append(sch.taskNodes, node)
			sch.taskNodeByNameIndex[node.Name] = node
		}
	}
	return nil
}

func (sch *defaultTaskScheduler) ListTask() []Task {
	ret := make([]Task, len(sch.taskNodes), len(sch.taskNodes))
	for i := range sch.taskNodes {
		ret[i] = sch.taskNodes[i]
	}
	return ret
}

func (sch *defaultTaskScheduler) StopTask() []Task {
	deletedTask := make([]Task, len(sch.taskNodes), len(sch.taskNodes))
	for i := range sch.taskNodes {
		if sch.taskNodes[i].DeletionTimestamp != nil {
			continue
		}
		sch.taskNodes[i].DeletionTimestamp = &metav1.Time{Time: timeNow()}
		deletedTask[i] = sch.taskNodes[i]
	}
	return deletedTask
}

func initTaskNodes(tasks []*api.Task) ([]*taskNode, error) {
	size := len(tasks)
	taskNodes := make([]*taskNode, size)
	for idx := 0; idx < size; idx++ {
		task := tasks[idx]
		tNode := &taskNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: task.Name,
			},
			Spec: taskSpec{
				Process:         task.Process,
				PodTemplateSpec: task.PodTemplateSpec,
			},
		}
		taskNodes[idx] = tNode
	}
	return taskNodes, nil
}

// collectTaskStatus from Pod via endpoint
func (sch *defaultTaskScheduler) collectTaskStatus(taskNodes []*taskNode) {
	ips := []string{}
	for _, tNode := range taskNodes {
		// unassigned no need to collect task status
		if tNode.IP == "" {
			continue
		}
		ips = append(ips, tNode.IP)
	}
	if len(ips) == 0 {
		return
	}
	tasks := sch.taskStatusCollector.Collect(context.Background(), ips)
	for _, tNode := range taskNodes {
		task, ok := tasks[tNode.IP]
		tNode.Status = task
		if ok && task != nil {
			tNode.transTaskState(parseTaskState(task), sch.logger)
		}
	}
}

func parseTaskState(task *api.Task) TaskState {
	if task.ProcessStatus != nil {
		return parseProcessTaskState(task.ProcessStatus)
	}
	if task.PodStatus != nil {
		return parsePodTaskState(task.PodStatus)
	}
	return UnknownTaskState
}

func parseProcessTaskState(status *api.ProcessStatus) TaskState {
	if status.Running != nil {
		return RunningTaskState
	} else if status.Terminated != nil {
		if status.Terminated.ExitCode == 0 {
			return SucceedTaskState
		} else {
			return FailedTaskState
		}
	}
	return UnknownTaskState
}

func parsePodTaskState(status *corev1.PodStatus) TaskState {
	switch status.Phase {
	case corev1.PodRunning:
		if utils.IsPodReadyConditionTrue(*status) {
			return RunningTaskState
		}
	case corev1.PodSucceeded:
		return SucceedTaskState
	case corev1.PodFailed:
		return FailedTaskState
	}
	return UnknownTaskState
}

func (sch *defaultTaskScheduler) scheduleTaskNodes() error {
	sch.freePods = assignTaskNodes(sch.taskNodes, sch.freePods, sch.logger)
	semaphore := make(chan struct{}, sch.maxConcurrency)
	var wg sync.WaitGroup
	for idx := range sch.taskNodes {
		tNode := sch.taskNodes[idx]
		semaphore <- struct{}{}
		wg.Add(1)
		go func(node *taskNode) {
			defer func() {
				<-semaphore
				wg.Done()
			}()
			scheduleSingleTaskNode(node, sch.taskClientCreator, sch.resPolicyWhenTaskComplete, sch.logger)
		}(tNode)
	}
	wg.Wait()
	return nil
}

// refreshFreePods updates the freePods slice based on allPods and currently assigned pods
// This ensures that each pod is only assigned to one taskNode
// Only pods with IP addresses are considered free for assignment
func (sch *defaultTaskScheduler) refreshFreePods() {
	// Create a map of assigned pod names for quick lookup
	assignedPods := make(map[string]bool, len(sch.allPods)/2)
	for _, tNode := range sch.taskNodes {
		if tNode.IP != "" && tNode.PodName != "" {
			assignedPods[tNode.PodName] = true
		}
	}
	// Rebuild freePods list with only unassigned pods that have IP addresses
	sch.freePods = make([]*corev1.Pod, 0, len(sch.allPods)/2)
	for _, pod := range sch.allPods {
		// Only consider pods with IP addresses as free for assignment
		if !assignedPods[pod.Name] && pod.Status.PodIP != "" {
			sch.freePods = append(sch.freePods, pod)
		}
	}
}

// assignTaskNodes handles all unassigned tasks in batch
func assignTaskNodes(taskNodes []*taskNode, freePods []*corev1.Pod, log logr.Logger) []*corev1.Pod {
	for _, tNode := range taskNodes {
		if len(freePods) == 0 {
			break
		}
		if tNode.IP != "" {
			continue
		}
		pod := freePods[0]
		log.Info("assign Pod to task node", "podName", pod.Name, "podNamespace", pod.Namespace, "podIP", pod.Status.PodIP, "taskName", tNode.Name)
		tNode.IP = pod.Status.PodIP
		tNode.PodName = pod.Name
		freePods = freePods[1:]
	}
	return freePods
}

func needRelease(tNode *taskNode, policy sandboxv1alpha1.TaskResourcePolicy) bool {
	if tNode.DeletionTimestamp != nil {
		return true
	}
	if policy == sandboxv1alpha1.TaskResourcePolicyRelease && tNode.isTaskCompleted() {
		return true
	}
	return false
}

// scheduleSingleTaskNode handles scheduling for a single task node based on its state
func scheduleSingleTaskNode(tNode *taskNode, taskClientCreator func(endpoint string) taskClient, resPolicyWhenTaskComplete sandboxv1alpha1.TaskResourcePolicy, log logr.Logger) {
	// pending
	if tNode.IP == "" {
		if tNode.DeletionTimestamp != nil {
			tNode.transSchState(stateReleased, log)
		}
	} else {
		// assigned
		if needRelease(tNode, resPolicyWhenTaskComplete) {
			tNode.transSchState(stateReleasing, log)
		} else {
			// no need to setTask if task is completed to avoid unnecessary network overhead
			if !tNode.isTaskCompleted() {
				task := &api.Task{
					Name:            tNode.Name,
					Process:         tNode.Spec.Process,
					PodTemplateSpec: tNode.Spec.PodTemplateSpec,
				}
				_, err := setTask(taskClientCreator(tNode.IP), task, log)
				if err != nil {
					log.Error(err, "Failed to set task", "taskName", tNode.Name, "endpoint", tNode.IP)
				}
			}
		}
	}
	if tNode.sState == stateReleasing {
		if tNode.isTaskDeleted() {
			tNode.transSchState(stateReleased, log)
		} else {
			_, err := setTask(taskClientCreator(tNode.IP), nil, log)
			if err != nil {
				log.Error(err, "Failed to notify executor about releasing task", "taskName", tNode.Name, "endpoint", tNode.IP)
			} else {
				log.Info("Successfully to notify client to release task", "taskName", tNode.Name, "endpoint", tNode.IP)
			}
		}
	}
}

func setTask(client taskClient, task *api.Task, log logr.Logger) (*api.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	verboseLog := log.V(3)
	if verboseLog.Enabled() {
		verboseLog.Info("client set task", "task", utils.DumpJSON(task))
	}
	return client.Set(ctx, task)
}
