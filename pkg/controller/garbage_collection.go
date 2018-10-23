package controller

import (
	"time"

	log "github.com/inconshreveable/log15"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	paddlelisters "github.com/baidu/paddle-on-k8s-operator/pkg/client/listers/paddlepaddle/v1alpha1"
	"github.com/baidu/paddle-on-k8s-operator/pkg/updater"
)

type GarbageCollector struct {
	kubeCli           kubernetes.Interface
	trainingjobLister paddlelisters.TrainingJobLister
}

func NewGarbageCollector(kubeCli kubernetes.Interface,
	trainingjobLister paddlelisters.TrainingJobLister) *GarbageCollector {
	return &GarbageCollector{
		kubeCli:           kubeCli,
		trainingjobLister: trainingjobLister,
	}
}

func (gc *GarbageCollector) CleanOrphans(d time.Duration) {
	ticker := time.NewTicker(d)
	for range ticker.C {
		log.Info("Garbage collector working now ...")
		gc.cleanOrphanReplicaSets()
		gc.cleanOrphanBatchJobs()
		gc.CleanGarbagePods()
	}
}

func (gc *GarbageCollector) cleanOrphanReplicaSets() {
	orphans, err := gc.findOrphanRelicaSets()
	if err != nil {
		log.Error("Find orphan replicasets", "error", err.Error())
		return
	}

	for _, orp := range orphans {
		log.Info("Cleaning orphan replicaset", "namespace", orp.Namespace, "replicaset", orp.Name)
		if err := gc.deleteReplicaSet(orp.Namespace, orp.Name); err != nil {
			log.Error("Clean orphan replicasets", "error", err.Error())
		}
	}
}

func (gc *GarbageCollector) cleanOrphanBatchJobs() {
	orphans, err := gc.findOrphanBatchJobs()
	if err != nil {
		log.Error("Find orphan batchjobs", "error", err.Error())
		return
	}

	for _, orp := range orphans {
		log.Info("Cleaning orphan batchjob", "namespace", orp.Namespace, "job", orp.Name)
		if err := gc.deleteBatchJob(orp.Namespace, orp.Name); err != nil {
			log.Error("Clean orphan batchjobs", "error", err.Error())
		}
	}
}

func (gc *GarbageCollector) findOrphanRelicaSets() ([]v1beta1.ReplicaSet, error) {
	orphans := make([]v1beta1.ReplicaSet, 0)
	all, err := gc.kubeCli.ExtensionsV1beta1().ReplicaSets("").List(metav1.ListOptions{})
	if err != nil {
		return orphans, err
	}

	for _, rs := range all.Items {
		lbs := updater.Labels(rs.GetLabels())
		isMaster, masterLab := lbs.HasLabel("paddle-job-master")
		isPserver, pserverLab := lbs.HasLabel("paddle-job-pserver")
		var jobName string
		if isMaster {
			jobName = masterLab
		} else if isPserver {
			jobName = pserverLab
		}
		if len(jobName) != 0 {
			found, err := gc.trainingJobFound(rs.Namespace, jobName)
			if err != nil {
				return orphans, err
			}
			if !found {
				log.Info("Found orphan replicaset", "namesapce", rs.Namespace, "name", rs.Name)
				orphans = append(orphans, rs)
			}
		}
	}

	return orphans, nil
}

func (gc *GarbageCollector) findOrphanBatchJobs() ([]batchv1.Job, error) {
	orphans := make([]batchv1.Job, 0)
	all, err := gc.kubeCli.BatchV1().Jobs("").List(metav1.ListOptions{})
	if err != nil {
		return orphans, err
	}

	for _, bj := range all.Items {
		lbs := updater.Labels(bj.GetLabels())
		isTrainer, trainerLabel := lbs.HasLabel("paddle-job")
		if isTrainer {
			found, err := gc.trainingJobFound(bj.Namespace, trainerLabel)
			if err != nil {
				return orphans, err
			}
			if !found {
				log.Info("Found orphan batchjob", "namespace", bj.Namespace, "name", bj.Name)
				orphans = append(orphans, bj)
			}
		}
	}

	return orphans, nil
}

func (gc *GarbageCollector) deleteReplicaSet(namespace, name string) error {
	obj, err := gc.kubeCli.ExtensionsV1beta1().ReplicaSets(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if *obj.Spec.Replicas != 0 {
		var replicas int32
		replicas = 0
		obj.Spec.Replicas = &replicas
		if _, err := gc.kubeCli.ExtensionsV1beta1().ReplicaSets(namespace).Update(obj); err != nil {
			return err
		}
	}

	err = gc.kubeCli.ExtensionsV1beta1().ReplicaSets(namespace).Delete(name, &metav1.DeleteOptions{})
	return err
}

func (gc *GarbageCollector) deleteBatchJob(namespace, name string) error {
	obj, err := gc.kubeCli.BatchV1().Jobs(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if *obj.Spec.Parallelism != 0 {
		var para int32
		para = 0
		obj.Spec.Parallelism = &para
		if _, err := gc.kubeCli.BatchV1().Jobs(namespace).Update(obj); err != nil {
			return err
		}
	}

	err = gc.kubeCli.BatchV1().Jobs(namespace).Delete(name, &metav1.DeleteOptions{})
	return err
}

func (gc *GarbageCollector) trainingJobFound(namespace, name string) (bool, error) {
	_, err := gc.trainingjobLister.TrainingJobs(namespace).Get(name)
	if err == nil {
		return true, nil
	}

	if apierrors.IsNotFound(err) {
		return false, nil
	}

	return false, err
}

func (gc *GarbageCollector) CleanGarbagePods() {
	all, err := gc.kubeCli.CoreV1().Pods("").List(metav1.ListOptions{})
	if err != nil {
		log.Error("List garbage pod failed")
		return
	}

	for _, pod := range all.Items {
		lbs := updater.Labels(pod.GetLabels())
		isPaddlePod, _ := lbs.HasLabel("paddle-job")

		if !isPaddlePod {
			continue
		}

		if pod.DeletionTimestamp != nil && pod.DeletionTimestamp.Time.Before(time.Now()) {
			log.Info("Find garbage pod", "name", pod.Name, "reason: terminated expired")
			var gracePeriodSeconds int64 = 0
			propagationPolicy := metav1.DeletePropagationBackground
			err = gc.kubeCli.CoreV1().Pods(pod.Namespace).Delete(pod.Name,
				&metav1.DeleteOptions{
					PropagationPolicy:  &propagationPolicy,
					GracePeriodSeconds: &gracePeriodSeconds,
				})
			if err != nil {
				log.Error("Delete garbage pod", "name", pod.Name, "reason", err.Error())
			}
			continue
		}

		containerStatus := false
		containers := pod.Status.ContainerStatuses
		for _, container := range containers {
			if !containerStatus && container.State.Waiting != nil && container.State.Waiting.Reason == "CreateContainerError" {
				containerStatus = false
				continue
			}

			containerStatus = true
			break
		}
		if !containerStatus {
			log.Error("Find garbage pod", "name", pod.Name, "reason", "CreateContainerError")
			err = gc.kubeCli.CoreV1().Pods(pod.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
			if err != nil {
				log.Error("Delete garbage pod", "name", pod.Name, "reason", err.Error())
			}
		}
	}
}