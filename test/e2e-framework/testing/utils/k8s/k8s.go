// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024-present Datadog, Inc.

// Package k8s contains utility functions for interacting with Kubernetes clusters.
package k8s

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/DataDog/datadog-agent/pkg/util/pointer"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	kubectlget "k8s.io/kubectl/pkg/cmd/get"
	kubectlutil "k8s.io/kubectl/pkg/cmd/util"
)

// CheckJobErrors checks if a job has failed and returns an error with more details about the failure, such as the pod that failed and the error code.
func CheckJobErrors(ctx context.Context, client kubernetes.Interface, namespace, jobName string) error {
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting job %s: %w", jobName, err)
	}

	if job.Status.Failed == 0 {
		return nil
	}

	// Get pod logs for debugging
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("job-name", jobName).String(),
		Limit:         1,
	})
	if err != nil {
		return fmt.Errorf("error listing pods for job %s: %w", jobName, err)
	}

	// Try to find a pod that failed to return a specific error about what happened
	for _, pod := range pods.Items {
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
				return fmt.Errorf("workload job %s failed: pod %s container %s exited with code %d: %s",
					jobName, pod.Name, containerStatus.Name,
					containerStatus.State.Terminated.ExitCode,
					containerStatus.State.Terminated.Message)
			}
		}
	}

	// Could not find a specific pod that failed, so return a generic error
	return fmt.Errorf("workload job %s failed. Kubernetes reports %d failed pods, but we could not find a specific one with the failure", jobName, job.Status.Failed)
}

func DumpK8sClusterState(ctx context.Context, kubeconfig *clientcmdapi.Config, out *strings.Builder) error {
	kubeconfigFile, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		return fmt.Errorf("failed to create kubeconfig temporary file: %v", err)
	}
	defer os.Remove(kubeconfigFile.Name())

	if err := clientcmd.WriteToFile(*kubeconfig, kubeconfigFile.Name()); err != nil {
		return fmt.Errorf("failed to write kubeconfig file: %v", err)
	}

	if err := kubeconfigFile.Close(); err != nil {
		return fmt.Errorf("failed to close kubeconfig file: %v", err)
	}

	fmt.Fprintf(out, "\n---------- All resources ----------\n")

	configFlags := genericclioptions.NewConfigFlags(false)
	kubeconfigFileName := kubeconfigFile.Name()
	configFlags.KubeConfig = &kubeconfigFileName

	factory := kubectlutil.NewFactory(configFlags)

	streams := genericiooptions.IOStreams{
		Out:    out,
		ErrOut: out,
	}

	getCmd := kubectlget.NewCmdGet("", factory, streams)
	getCmd.SetOut(out)
	getCmd.SetErr(out)
	getCmd.SetContext(ctx)
	getCmd.SetArgs([]string{
		"nodes,all",
		"--all-namespaces",
		"-o",
		"wide",
	})
	if err := getCmd.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("failed to execute kubectl get: %v", err)
	}

	// Get the logs of containers that have restarted
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigFile.Name())
	if err != nil {
		fmt.Fprintf(out, "Failed to build Kubernetes config: %v\n", err)
		return nil
	}
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(out, "Failed to create Kubernetes client: %v\n", err)
		return nil
	}

	pods, err := k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(out, "Failed to list pods: %v\n", err)
		return nil
	}

	for _, pod := range pods.Items {
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.RestartCount > 0 || !containerStatus.Ready {
				fmt.Fprintf(out, "\nLOGS FOR POD %s/%s CONTAINER %s:\n", pod.Namespace, pod.Name, containerStatus.Name)
				logs, err := k8sClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
					Container: containerStatus.Name,
					Previous:  true,
					TailLines: pointer.Ptr(int64(100)),
				}).Stream(ctx)
				if err != nil {
					fmt.Fprintf(out, "Failed to get logs: %v\n", err)
					continue
				}

				_, err = io.Copy(out, logs)
				logs.Close()
				if err != nil {
					fmt.Fprintf(out, "Failed to copy logs: %v\n", err)
					continue
				}
			}
		}
	}
	return nil
}

// terminalWaitingReasons are container waiting reasons that indicate the pod will not recover without intervention.
var terminalWaitingReasons = map[string]bool{
	"ImagePullBackOff":  true,
	"ErrImagePull":      true,
	"InvalidImageName":  true,
	"CrashLoopBackOff":  true,
	"CreateContainerConfigError": true,
}

// WaitForJobPodRunning polls until a Job's Pod leaves the Pending phase or a terminal
// error is detected. It returns the Pod on success, or an error describing why the Pod
// could not start (image pull failure, scheduling issue, crash loop, or timeout).
func WaitForJobPodRunning(ctx context.Context, client kubernetes.Interface, namespace, jobName string, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fields.OneTermEqualSelector("job-name", jobName).String(),
		})
		if err != nil {
			return nil, fmt.Errorf("error listing pods for job %s: %w", jobName, err)
		}

		for i := range pods.Items {
			pod := &pods.Items[i]

			if pod.Status.Phase != corev1.PodPending {
				return pod, nil
			}

			// Check init container and regular container statuses for terminal errors
			allStatuses := append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...)
			for _, cs := range allStatuses {
				if cs.State.Waiting != nil && terminalWaitingReasons[cs.State.Waiting.Reason] {
					return nil, fmt.Errorf("job %s pod %s container %s: %s - %s",
						jobName, pod.Name, cs.Name,
						cs.State.Waiting.Reason, cs.State.Waiting.Message)
				}
			}

			// Check for unschedulable conditions
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable {
					return nil, fmt.Errorf("job %s pod %s unschedulable: %s", jobName, pod.Name, cond.Message)
				}
			}
		}

		if time.Now().After(deadline) {
			if len(pods.Items) == 0 {
				return nil, fmt.Errorf("job %s: no pods created within %s", jobName, timeout)
			}
			pod := &pods.Items[0]
			return nil, fmt.Errorf("job %s pod %s still pending after %s (phase=%s, reason=%s, message=%s)",
				jobName, pod.Name, timeout, pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// DescribeJob collects diagnostic information about a Job and its Pods into a human-readable
// string. It is designed for use in failure messages and never returns an error — each
// section is best-effort.
func DescribeJob(ctx context.Context, client kubernetes.Interface, namespace, jobName string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "--- Job %s/%s diagnostics ---\n", namespace, jobName)

	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(&out, "Error getting job: %v\n", err)
		return out.String()
	}
	fmt.Fprintf(&out, "Job status: active=%d succeeded=%d failed=%d\n",
		job.Status.Active, job.Status.Succeeded, job.Status.Failed)
	for _, cond := range job.Status.Conditions {
		fmt.Fprintf(&out, "  condition: type=%s status=%s reason=%s message=%s\n",
			cond.Type, cond.Status, cond.Reason, cond.Message)
	}

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("job-name", jobName).String(),
	})
	if err != nil {
		fmt.Fprintf(&out, "Error listing pods: %v\n", err)
		return out.String()
	}

	for _, pod := range pods.Items {
		fmt.Fprintf(&out, "\nPod %s: phase=%s reason=%s message=%s\n",
			pod.Name, pod.Status.Phase, pod.Status.Reason, pod.Status.Message)

		for _, cond := range pod.Status.Conditions {
			if cond.Status == corev1.ConditionFalse {
				fmt.Fprintf(&out, "  condition: type=%s reason=%s message=%s\n",
					cond.Type, cond.Reason, cond.Message)
			}
		}

		allStatuses := append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...)
		for _, cs := range allStatuses {
			fmt.Fprintf(&out, "  container %s: ", cs.Name)
			switch {
			case cs.State.Running != nil:
				fmt.Fprintf(&out, "running since %s\n", cs.State.Running.StartedAt.Time)
			case cs.State.Terminated != nil:
				fmt.Fprintf(&out, "terminated: exitCode=%d reason=%s message=%s\n",
					cs.State.Terminated.ExitCode, cs.State.Terminated.Reason, cs.State.Terminated.Message)
			case cs.State.Waiting != nil:
				fmt.Fprintf(&out, "waiting: reason=%s message=%s\n",
					cs.State.Waiting.Reason, cs.State.Waiting.Message)
			default:
				fmt.Fprintf(&out, "unknown state\n")
			}
		}

		// Pod events
		events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("involvedObject.name", pod.Name).String(),
		})
		if err == nil && len(events.Items) > 0 {
			fmt.Fprintf(&out, "  events:\n")
			for _, event := range events.Items {
				fmt.Fprintf(&out, "    %s %s: %s\n", event.Reason, event.Type, event.Message)
			}
		}

		// Container logs (best-effort, short tail)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil || cs.State.Running != nil {
				logStream, err := client.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
					Container: cs.Name,
					TailLines: pointer.Ptr(int64(20)),
				}).Stream(ctx)
				if err != nil {
					fmt.Fprintf(&out, "  logs for %s: error: %v\n", cs.Name, err)
					continue
				}
				var logBuf strings.Builder
				_, _ = io.Copy(&logBuf, logStream)
				logStream.Close()
				if logBuf.Len() > 0 {
					fmt.Fprintf(&out, "  logs for %s:\n%s\n", cs.Name, logBuf.String())
				}
			}
		}
	}

	return out.String()
}
