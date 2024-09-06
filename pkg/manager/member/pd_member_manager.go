// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/pingcap/tidb-operator/pkg/apis/label"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/manager"
	"github.com/pingcap/tidb-operator/pkg/manager/member/constants"
	"github.com/pingcap/tidb-operator/pkg/manager/member/startscript"
	"github.com/pingcap/tidb-operator/pkg/manager/suspender"
	mngerutils "github.com/pingcap/tidb-operator/pkg/manager/utils"
	"github.com/pingcap/tidb-operator/pkg/manager/volumes"
	"github.com/pingcap/tidb-operator/pkg/third_party/k8s"
	"github.com/pingcap/tidb-operator/pkg/util"

	"github.com/Masterminds/semver"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

const (
	// pdClusterCertPath is where the cert for inter-cluster communication stored (if any)
	pdClusterCertPath  = "/var/lib/pd-tls"
	tidbClientCertPath = "/var/lib/tidb-client-tls"

	//find a better way to manage store only managed by pd in Operator
	pdMemberLimitPattern = `%s-pd-\d+\.%s-pd-peer\.%s\.svc%s\:\d+`
)

type pdMemberManager struct {
	deps              *controller.Dependencies
	scaler            Scaler
	upgrader          Upgrader
	failover          Failover
	suspender         suspender.Suspender
	podVolumeModifier volumes.PodVolumeModifier
}

// NewPDMemberManager returns a *pdMemberManager
func NewPDMemberManager(dependencies *controller.Dependencies, pdScaler Scaler, pdUpgrader Upgrader, pdFailover Failover, spder suspender.Suspender, pvm volumes.PodVolumeModifier) manager.Manager {
	return &pdMemberManager{
		deps:              dependencies,
		scaler:            pdScaler,
		upgrader:          pdUpgrader,
		failover:          pdFailover,
		suspender:         spder,
		podVolumeModifier: pvm,
	}
}

// Sync 进行pd组件的sync操作
func (m *pdMemberManager) Sync(tc *v1alpha1.TidbCluster) error {
	// If pd is not specified return
	if tc.Spec.PD == nil {
		return nil
	}

	if (tc.Spec.PD.Mode == "ms" && tc.Spec.PDMS == nil) ||
		(tc.Spec.PDMS != nil && tc.Spec.PD.Mode != "ms") {
		klog.Infof("tidbcluster: [%s/%s]'s enable micro service failed, please check `PD.Mode` and `PDMS`", tc.GetNamespace(), tc.GetName())
	}

	// skip sync if pd is suspended
	component := v1alpha1.PDMemberType
	needSuspend, err := m.suspender.SuspendComponent(tc, component) //判断tc里面有没有配置pause
	if err != nil {
		return fmt.Errorf("suspend %s failed: %v", component, err)
	}
	if needSuspend { //pause了就直接return
		klog.Infof("component %s for cluster %s/%s is suspended, skip syncing", component, tc.GetNamespace(), tc.GetName())
		return nil
	}

	// Sync PD Service
	// 第一步：sync pd的service资源。前两个步骤都是sync service方面的配置，第三步才是搞pd的sts，但其实我理解这个步骤的顺序并不重要，也可以先弄sts，然后配service，毕竟kube proxy配置iptables是动态的，pod running之后就会自动加入到service的endpoint里面去
	if err := m.syncPDServiceForTidbCluster(tc); err != nil {
		return err
	}

	// Sync PD Headless Service
	// 第二步：sync pd的headless资源。这种类型的资源一般是用于需要知道具体是哪个pd 的pod来服务的
	if err := m.syncPDHeadlessServiceForTidbCluster(tc); err != nil {
		return err
	}

	// Sync PD StatefulSet
	// *******第三步：sync pd的sts******
	return m.syncPDStatefulSetForTidbCluster(tc)
}

func (m *pdMemberManager) syncPDServiceForTidbCluster(tc *v1alpha1.TidbCluster) error {
	if tc.Spec.Paused {
		klog.V(4).Infof("tidb cluster %s/%s is paused, skip syncing for pd service", tc.GetNamespace(), tc.GetName())
		return nil
	}

	ns := tc.GetNamespace()
	tcName := tc.GetName()

	newSvc := m.getNewPDServiceForTidbCluster(tc)
	oldSvcTmp, err := m.deps.ServiceLister.Services(ns).Get(controller.PDMemberName(tcName))
	if errors.IsNotFound(err) {
		err = controller.SetServiceLastAppliedConfigAnnotation(newSvc)
		if err != nil {
			return err
		}
		return m.deps.ServiceControl.CreateService(tc, newSvc) //到k8s集群创建真实的service资源出来
	}
	if err != nil {
		return fmt.Errorf("syncPDServiceForTidbCluster: failed to get svc %s for cluster %s/%s, error: %s", controller.PDMemberName(tcName), ns, tcName, err)
	}

	oldSvc := oldSvcTmp.DeepCopy()

	_, err = m.deps.ServiceControl.SyncComponentService(
		tc,
		newSvc,
		oldSvc,
		true)

	if err != nil {
		return err
	}

	return nil
}

func (m *pdMemberManager) syncPDHeadlessServiceForTidbCluster(tc *v1alpha1.TidbCluster) error {
	if tc.Spec.Paused {
		klog.V(4).Infof("tidb cluster %s/%s is paused, skip syncing for pd headless service", tc.GetNamespace(), tc.GetName())
		return nil
	}

	ns := tc.GetNamespace()
	tcName := tc.GetName()

	newSvc := getNewPDHeadlessServiceForTidbCluster(tc)
	oldSvcTmp, err := m.deps.ServiceLister.Services(ns).Get(controller.PDPeerMemberName(tcName))
	if errors.IsNotFound(err) {
		err = controller.SetServiceLastAppliedConfigAnnotation(newSvc)
		if err != nil {
			return err
		}
		return m.deps.ServiceControl.CreateService(tc, newSvc)
	}
	if err != nil {
		return fmt.Errorf("syncPDHeadlessServiceForTidbCluster: failed to get svc %s for cluster %s/%s, error: %s", controller.PDPeerMemberName(tcName), ns, tcName, err)
	}

	oldSvc := oldSvcTmp.DeepCopy()

	_, err = m.deps.ServiceControl.SyncComponentService(
		tc,
		newSvc,
		oldSvc,
		false)

	if err != nil {
		return err
	}

	return nil
}

func (m *pdMemberManager) syncPDStatefulSetForTidbCluster(tc *v1alpha1.TidbCluster) error {
	ns := tc.GetNamespace()
	tcName := tc.GetName()

	oldPDSetTmp, err := m.deps.StatefulSetLister.StatefulSets(ns).Get(controller.PDMemberName(tcName)) //获取到ns下面的pd的sts
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("syncPDStatefulSetForTidbCluster: fail to get sts %s for cluster %s/%s, error: %s", controller.PDMemberName(tcName), ns, tcName, err)
	}
	setNotExist := errors.IsNotFound(err) //是否存在老的sts（也就是说判断是新建实例的过程还是说改现有实例tc的过程）

	oldPDSet := oldPDSetTmp.DeepCopy()

	if err := m.syncTidbClusterStatus(tc, oldPDSet); err != nil { //从pd里面获取到member的最新信息然后更新到tc
		klog.Errorf("failed to sync TidbCluster: [%s/%s]'s status, error: %v", ns, tcName, err)
	}

	//如果暂停就不sync了
	if tc.Spec.Paused {
		klog.V(4).Infof("tidb cluster %s/%s is paused, skip syncing for pd statefulset", tc.GetNamespace(), tc.GetName())
		return nil
	}

	//tc是最新的了，然后就开始把tc的最新配置应用到pd下面的各个组件呗，比如cm、sts、pvc啥的

	//1.更新cm，cm里面包含了pd的启动脚本
	cm, err := m.syncPDConfigMap(tc, oldPDSet)
	if err != nil {
		return err
	}

	//构建pd的sts出来
	newPDSet, err := getNewPDSetForTidbCluster(tc, cm)
	if err != nil {
		return err
	}

	if setNotExist { //新建实例的话
		err = mngerutils.SetStatefulSetLastAppliedConfigAnnotation(newPDSet)
		if err != nil {
			return err
		}
		if err := m.deps.StatefulSetControl.CreateStatefulSet(tc, newPDSet); err != nil { //创建pd的sts
			return err
		}
		tc.Status.PD.StatefulSet = &apps.StatefulSetStatus{}
		return controller.RequeueErrorf("TidbCluster: [%s/%s], waiting for PD cluster running", ns, tcName) //直接返回
	}

	// 下面的逻辑是针对已经存在的tidb实例进行更新tc、sts配置的过程
	// Force update takes precedence over scaling because force upgrade won't take effect when cluster gets stuck at scaling
	// 让tidb版本升级的逻辑优先级高于变配，因为变配过程没做完的话，升级也不会进行

	//1、处理升级逻辑
	if !tc.Status.PD.Synced && !templateEqual(newPDSet, oldPDSet) {
		// upgrade forced only when `Synced` is false, because unable to upgrade gracefully
		/*
				升级过程中pd的status信息:
			pd:
			    image: hub.jdcloud.com/tidb/pd:v6.5.8-05f94b2
			    leader:
			      clientURL: http://tidb-4cqy4tefep-pd-0.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379
			      health: true
			      id: "2197499305875197257"
			      lastTransitionTime: "2024-08-08T08:33:44Z"
			      name: tidb-4cqy4tefep-pd-0
			    members:
			      tidb-4cqy4tefep-pd-0:
			        clientURL: http://tidb-4cqy4tefep-pd-0.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379
			        health: true
			        id: "2197499305875197257"
			        lastTransitionTime: "2024-08-08T08:33:44Z"
			        name: tidb-4cqy4tefep-pd-0
			      tidb-4cqy4tefep-pd-1:
			        clientURL: http://tidb-4cqy4tefep-pd-1.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379
			        health: true
			        id: "13124242843153742462"
			        lastTransitionTime: "2024-08-08T08:33:44Z"
			        name: tidb-4cqy4tefep-pd-1
			      tidb-4cqy4tefep-pd-2:
			        clientURL: http://tidb-4cqy4tefep-pd-2.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379
			        health: false
			        id: "3978176930332030335"
			        lastTransitionTime: "2024-09-06T09:25:11Z"
			        name: tidb-4cqy4tefep-pd-2
			    phase: Upgrade *****************************************这里********************************************
			    statefulSet:
			      collisionCount: 0
			      currentReplicas: 2
			      currentRevision: tidb-4cqy4tefep-pd-5784cb95b4
			      observedGeneration: 3
			      readyReplicas: 2
			      replicas: 3
			      updateRevision: tidb-4cqy4tefep-pd-6978dc5954
			      updatedReplicas: 1
			    synced: true
			    volumes:
			      pd:
			        boundCount: 3
			        currentCapacity: 100Gi
			        currentCount: 3
			        currentStorageClass: jdcloud-lvm
			        modifiedCapacity: 100Gi
			        modifiedCount: 3
			        modifiedStorageClass: jdcloud-lvm
			        name: pd
			        resizedCapacity: 100Gi
			        resizedCount: 3

		*/
		forceUpgradeAnnoSet := NeedForceUpgrade(tc.Annotations)                        //判断是否进行升级操作
		onlyOnePD := *oldPDSet.Spec.Replicas < 2 && len(tc.Status.PD.PeerMembers) == 0 // it's acceptable to use old record about peer members

		if forceUpgradeAnnoSet || onlyOnePD {
			tc.Status.PD.Phase = v1alpha1.UpgradePhase //把pd状态设置为升级
			mngerutils.SetUpgradePartition(newPDSet, 0)
			errSTS := mngerutils.UpdateStatefulSet(m.deps.StatefulSetControl, tc, newPDSet, oldPDSet)
			return controller.RequeueErrorf("tidbcluster: [%s/%s]'s pd needs force upgrade, %v", ns, tcName, errSTS)
		}
	}

	// Scaling takes precedence over upgrading because:
	// - if a pd fails in the upgrading, users may want to delete it or add
	//   new replicas
	// - it's ok to scale in the middle of upgrading (in statefulset controller
	//   scaling takes precedence over upgrading too)
	// 扩展优先于升级，因为:
	// —如果pd升级失败，用户可能需要删除pd或添加新的副本
	// -可以在升级过程中进行扩展(在statefulset中，控制器的扩展优先于升级)
	// 2、 处理变配逻辑
	if err := m.scaler.Scale(tc, oldPDSet, newPDSet); err != nil {
		return err
	}

	// 3、故障转移逻辑
	if m.deps.CLIConfig.AutoFailover {
		if m.shouldRecover(tc) {
			m.failover.Recover(tc)
		} else if tc.Spec.PD.MaxFailoverCount != nil && *tc.Spec.PD.MaxFailoverCount > 0 && (tc.PDAllPodsStarted() && !tc.PDAllMembersReady() || tc.PDAutoFailovering()) {
			if err := m.failover.Failover(tc); err != nil {
				return err
			}
		}
	}

	if tc.Status.PD.VolReplaceInProgress {
		// Volume Replace in Progress, so do not make any changes to Sts spec, overwrite with old pod spec
		// config as we are not ready to upgrade yet.
		// 卷替换正在进行中，所以不要对Sts规范做任何更改，用旧的pod规范配置覆盖，因为我们还没有准备好升级。
		_, podSpec, err := GetLastAppliedConfig(oldPDSet)
		if err != nil {
			return err
		}
		newPDSet.Spec.Template.Spec = *podSpec
	}

	if !templateEqual(newPDSet, oldPDSet) || tc.Status.PD.Phase == v1alpha1.UpgradePhase {
		if err := m.upgrader.Upgrade(tc, oldPDSet, newPDSet); err != nil {
			return err
		}
	}

	return mngerutils.UpdateStatefulSetWithPrecheck(m.deps, tc, "FailedUpdatePDSTS", newPDSet, oldPDSet)
}

// shouldRecover checks whether we should perform recovery operation.
func (m *pdMemberManager) shouldRecover(tc *v1alpha1.TidbCluster) bool {
	if tc.Status.PD.FailureMembers == nil {
		return false
	}
	// If all desired replicas (excluding failover pods) of tidb cluster are
	// healthy, we can perform our failover recovery operation.
	// Note that failover pods may fail (e.g. lack of resources) and we don't care
	// about them because we're going to delete them.
	for ordinal := range tc.PDStsDesiredOrdinals(true) {
		name := fmt.Sprintf("%s-%d", controller.PDMemberName(tc.GetName()), ordinal)
		pod, err := m.deps.PodLister.Pods(tc.Namespace).Get(name)
		if err != nil {
			klog.Errorf("pod %s/%s does not exist: %v", tc.Namespace, name, err)
			return false
		}
		if !k8s.IsPodReady(pod) {
			return false
		}
		ok := false
		for pdName, pdMember := range tc.Status.PD.Members {
			if strings.Split(pdName, ".")[0] == pod.Name {
				if !pdMember.Health {
					return false
				}
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (m *pdMemberManager) syncTidbClusterStatus(tc *v1alpha1.TidbCluster, set *apps.StatefulSet) error {
	if set == nil {
		// skip if not created yet
		return nil
	}

	ns := tc.GetNamespace()
	tcName := tc.GetName()

	tc.Status.PD.StatefulSet = &set.Status

	upgrading, err := m.pdStatefulSetIsUpgrading(set, tc)
	if err != nil {
		return err
	}

	// Scaling takes precedence over upgrading.
	//更新tc里面pd的状态，这一步很重要哦。因为后续对于pd的扩缩容、Failover、升级等操作都会依赖tc中的Status字段的值来进行决策。
	if tc.PDStsDesiredReplicas() != *set.Spec.Replicas {
		tc.Status.PD.Phase = v1alpha1.ScalePhase //扩缩容
	} else if upgrading {
		tc.Status.PD.Phase = v1alpha1.UpgradePhase //升级
	} else {
		tc.Status.PD.Phase = v1alpha1.NormalPhase
	}

	pdClient := controller.GetPDClient(m.deps.PDControl, tc) //获取pd client，其实是个pd的svc域名

	healthInfo, err := pdClient.GetHealth() //获取集群健康度
	/*
			[
		  {
		    "name": "tidb-4cqy4tefep-pd-0",
		    "member_id": 2197499305875197257,
		    "client_urls": [
		      "http://tidb-4cqy4tefep-pd-0.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379"
		    ],
		    "health": true
		  },
		  {
		    "name": "tidb-4cqy4tefep-pd-2",
		    "member_id": 3978176930332030335,
		    "client_urls": [
		      "http://tidb-4cqy4tefep-pd-2.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379"
		    ],
		    "health": true
		  },
		  {
		    "name": "tidb-4cqy4tefep-pd-1",
		    "member_id": 13124242843153742462,
		    "client_urls": [
		      "http://tidb-4cqy4tefep-pd-1.tidb-4cqy4tefep-pd-peer.tidb-4cqy4tefep.svc:2379"
		    ],
		    "health": true
		  }
		]
	*/

	if err != nil {
		tc.Status.PD.Synced = false
		// get endpoints info
		eps, epErr := m.deps.EndpointLister.Endpoints(ns).Get(controller.PDMemberName(tcName))
		if epErr != nil {
			return fmt.Errorf("syncTidbClusterStatus: failed to get endpoints %s for cluster %s/%s, err: %s, epErr %s", controller.PDMemberName(tcName), ns, tcName, err, epErr)
		}
		// pd service has no endpoints
		if eps != nil && len(eps.Subsets) == 0 {
			return fmt.Errorf("%s, service %s/%s has no endpoints", err, ns, controller.PDMemberName(tcName))
		}
		return err
	}

	/*
			{
		  	"id": 7400683975866343221,
		  	"max_peer_count": 3
			}
	*/
	cluster, err := pdClient.GetCluster()
	if err != nil {
		tc.Status.PD.Synced = false
		return err
	}
	tc.Status.ClusterID = strconv.FormatUint(cluster.Id, 10) //设置tc里面status中的集群id信息，从pd内部获取来
	leader, err := pdClient.GetPDLeader()                    //获取pd leader
	if err != nil {
		tc.Status.PD.Synced = false
		return err
	}

	rePDMembers, err := regexp.Compile(fmt.Sprintf(pdMemberLimitPattern, tc.Name, tc.Name, tc.Namespace, controller.FormatClusterDomainForRegex(tc.Spec.ClusterDomain)))
	if err != nil {
		return err
	}
	pdStatus := map[string]v1alpha1.PDMember{}
	peerPDStatus := map[string]v1alpha1.PDMember{}
	for _, memberHealth := range healthInfo.Healths { // 这块逻辑就是从pd里面获取到各个member信息，然后更新到tc的status里面去
		memberID := memberHealth.MemberID
		var clientURL string
		if len(memberHealth.ClientUrls) > 0 {
			clientURL = memberHealth.ClientUrls[0]
		}
		name := memberHealth.Name
		if len(name) == 0 {
			klog.Warningf("PD member: [%d] doesn't have a name, and can't get it from clientUrls: [%s], memberHealth Info: [%v] in [%s/%s]",
				memberID, memberHealth.ClientUrls, memberHealth, ns, tcName)
			continue
		}

		status := v1alpha1.PDMember{
			Name:      name,
			ID:        fmt.Sprintf("%d", memberID),
			ClientURL: clientURL,
			Health:    memberHealth.Health,
		}
		status.LastTransitionTime = metav1.Now()

		// matching `rePDMembers` means `clientURL` is a PD in current tc
		if rePDMembers.Match([]byte(clientURL)) {
			oldPDMember, exist := tc.Status.PD.Members[name]
			if exist && status.Health == oldPDMember.Health {
				status.LastTransitionTime = oldPDMember.LastTransitionTime
			}
			pdStatus[name] = status
		} else {
			oldPDMember, exist := tc.Status.PD.PeerMembers[name]
			if exist && status.Health == oldPDMember.Health {
				status.LastTransitionTime = oldPDMember.LastTransitionTime
			}
			peerPDStatus[name] = status
		}

		if name == leader.GetName() {
			tc.Status.PD.Leader = status
		}
	}

	tc.Status.PD.Synced = true //sync完了设置为true
	tc.Status.PD.Members = pdStatus
	tc.Status.PD.PeerMembers = peerPDStatus
	tc.Status.PD.Image = ""
	if c := findContainerByName(set, "pd"); c != nil {
		tc.Status.PD.Image = c.Image
	}

	if err := m.collectUnjoinedMembers(tc, set, pdStatus); err != nil {
		return err
	}

	err = volumes.SyncVolumeStatus(m.podVolumeModifier, m.deps.PodLister, tc, v1alpha1.PDMemberType)
	if err != nil {
		return fmt.Errorf("failed to sync volume status for pd: %v", err)
	}
	return nil
}

// syncPDConfigMap syncs the configmap of PD
func (m *pdMemberManager) syncPDConfigMap(tc *v1alpha1.TidbCluster, set *apps.StatefulSet) (*corev1.ConfigMap, error) {
	// For backward compatibility, only sync tidb configmap when .pd.config is non-nil
	if tc.Spec.PD.Config == nil {
		return nil, nil
	}
	newCm, err := getPDConfigMap(tc)
	if err != nil {
		return nil, err
	}

	var inUseName string
	if set != nil {
		inUseName = mngerutils.FindConfigMapVolume(&set.Spec.Template.Spec, func(name string) bool {
			return strings.HasPrefix(name, controller.PDMemberName(tc.Name))
		})
	} else {
		inUseName, err = mngerutils.FindConfigMapNameFromTCAnno(context.Background(), m.deps.ConfigMapLister, tc, v1alpha1.PDMemberType, newCm)
		if err != nil {
			return nil, err
		}
	}

	err = mngerutils.UpdateConfigMapIfNeed(m.deps.ConfigMapLister, tc.BasePDSpec().ConfigUpdateStrategy(), inUseName, newCm)
	if err != nil {
		return nil, err
	}
	return m.deps.TypedControl.CreateOrUpdateConfigMap(tc, newCm) //创建或者更新cm
}

func (m *pdMemberManager) getNewPDServiceForTidbCluster(tc *v1alpha1.TidbCluster) *corev1.Service {
	ns := tc.Namespace
	tcName := tc.Name
	svcName := controller.PDMemberName(tcName)
	instanceName := tc.GetInstanceName()
	pdSelector := label.New().Instance(instanceName).PD()
	pdLabels := pdSelector.Copy().UsedByEndUser().Labels()

	//组装pd service的yaml信息
	pdService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            svcName,
			Namespace:       ns,
			Labels:          pdLabels,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Spec: corev1.ServiceSpec{
			Type: controller.GetServiceType(tc.Spec.Services, v1alpha1.PDMemberType.String()),
			Ports: []corev1.ServicePort{
				{
					Name:       "client",
					Port:       v1alpha1.DefaultPDClientPort,
					TargetPort: intstr.FromInt(int(v1alpha1.DefaultPDClientPort)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: pdSelector.Labels(),
		},
	}

	// override fields with user-defined ServiceSpec
	svcSpec := tc.Spec.PD.Service
	if svcSpec != nil { //看要不要从tc里面拿service信息
		if svcSpec.Type != "" {
			pdService.Spec.Type = svcSpec.Type
		}
		pdService.ObjectMeta.Annotations = util.CopyStringMap(svcSpec.Annotations)
		pdService.ObjectMeta.Labels = util.CombineStringMap(pdService.ObjectMeta.Labels, svcSpec.Labels)
		if svcSpec.LoadBalancerIP != nil {
			pdService.Spec.LoadBalancerIP = *svcSpec.LoadBalancerIP
		}
		if svcSpec.ClusterIP != nil {
			pdService.Spec.ClusterIP = *svcSpec.ClusterIP
		}
		if svcSpec.PortName != nil {
			pdService.Spec.Ports[0].Name = *svcSpec.PortName
		}
	}

	if tc.Spec.PreferIPv6 {
		SetServiceWhenPreferIPv6(pdService)
	}

	return pdService
}

func getNewPDHeadlessServiceForTidbCluster(tc *v1alpha1.TidbCluster) *corev1.Service {
	ns := tc.Namespace
	tcName := tc.Name
	svcName := controller.PDPeerMemberName(tcName)
	instanceName := tc.GetInstanceName()
	pdSelector := label.New().Instance(instanceName).PD()
	pdLabels := pdSelector.Copy().UsedByPeer().Labels()

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            svcName,
			Namespace:       ns,
			Labels:          pdLabels,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{
					Name:       fmt.Sprintf("tcp-peer-%d", v1alpha1.DefaultPDPeerPort),
					Port:       v1alpha1.DefaultPDPeerPort,
					TargetPort: intstr.FromInt(int(v1alpha1.DefaultPDPeerPort)),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       fmt.Sprintf("tcp-peer-%d", v1alpha1.DefaultPDClientPort),
					Port:       v1alpha1.DefaultPDClientPort,
					TargetPort: intstr.FromInt(int(v1alpha1.DefaultPDClientPort)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector:                 pdSelector.Labels(),
			PublishNotReadyAddresses: true,
		},
	}

	if tc.Spec.PreferIPv6 {
		SetServiceWhenPreferIPv6(svc)
	}

	return svc
}

func (m *pdMemberManager) pdStatefulSetIsUpgrading(set *apps.StatefulSet, tc *v1alpha1.TidbCluster) (bool, error) {
	if mngerutils.StatefulSetIsUpgrading(set) {
		return true, nil
	}
	instanceName := tc.GetInstanceName()
	selector, err := label.New().
		Instance(instanceName).
		PD().
		Selector()
	if err != nil {
		return false, err
	}
	pdPods, err := m.deps.PodLister.Pods(tc.GetNamespace()).List(selector)
	if err != nil {
		return false, fmt.Errorf("pdStatefulSetIsUpgrading: failed to list pods for cluster %s/%s, selector %s, error: %v", tc.GetNamespace(), instanceName, selector, err)
	}
	for _, pod := range pdPods {
		revisionHash, exist := pod.Labels[apps.ControllerRevisionHashLabelKey]
		if !exist {
			return false, nil
		}
		if revisionHash != tc.Status.PD.StatefulSet.UpdateRevision {
			return true, nil
		}
	}
	return false, nil
}

func getNewPDSetForTidbCluster(tc *v1alpha1.TidbCluster, cm *corev1.ConfigMap) (*apps.StatefulSet, error) {
	ns := tc.Namespace
	tcName := tc.Name
	basePDSpec := tc.BasePDSpec()
	instanceName := tc.GetInstanceName()
	pdConfigMap := controller.MemberConfigMapName(tc, v1alpha1.PDMemberType)
	if cm != nil {
		pdConfigMap = cm.Name
	}

	//operator要求tidb的版本得大于等于4
	clusterVersionGE4, err := clusterVersionGreaterThanOrEqualTo4(tc.PDVersion(), tc.Spec.PD.Mode)
	if err != nil {
		klog.V(4).Infof("cluster version: %s is not semantic versioning compatible", tc.PDVersion())
	}

	annMount, annVolume := annotationsMountVolume() //把pd pod的metadata.annotation注入到pd容器里面去。返回volume mount mount和volume
	dataVolumeName := string(v1alpha1.GetStorageVolumeName("", v1alpha1.PDMemberType))
	volMounts := []corev1.VolumeMount{ //挂载这些个东西到pod里面去
		annMount,
		{Name: "config", ReadOnly: true, MountPath: "/etc/pd"},
		{Name: "startup-script", ReadOnly: true, MountPath: "/usr/local/bin"},
		{Name: dataVolumeName, MountPath: constants.PDDataVolumeMountPath},
	}

	//===========tls安全部分跳过不看
	if tc.IsTLSClusterEnabled() {
		volMounts = append(volMounts, corev1.VolumeMount{
			Name: "pd-tls", ReadOnly: true, MountPath: "/var/lib/pd-tls",
		})
		if tc.Spec.PD.MountClusterClientSecret != nil && *tc.Spec.PD.MountClusterClientSecret {
			volMounts = append(volMounts, corev1.VolumeMount{
				Name: util.ClusterClientVolName, ReadOnly: true, MountPath: util.ClusterClientTLSPath,
			})
		}
	}
	if tc.Spec.TiDB != nil && tc.Spec.TiDB.IsTLSClientEnabled() && !tc.SkipTLSWhenConnectTiDB() && clusterVersionGE4 {
		volMounts = append(volMounts, corev1.VolumeMount{
			Name: "tidb-client-tls", ReadOnly: true, MountPath: tidbClientCertPath,
		})
	}
	//===========tls安全部分跳过不看

	vols := []corev1.Volume{
		annVolume,
		{Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: pdConfigMap,
					},
					Items: []corev1.KeyToPath{{Key: "config-file", Path: "pd.toml"}},
				},
			},
		},
		{Name: "startup-script",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: pdConfigMap,
					},
					Items: []corev1.KeyToPath{{Key: "startup-script", Path: "pd_start_script.sh"}},
				},
			},
		},
	}
	if tc.IsTLSClusterEnabled() {
		vols = append(vols, corev1.Volume{
			Name: "pd-tls", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: util.ClusterTLSSecretName(tc.Name, label.PDLabelVal),
				},
			},
		})
		if tc.Spec.PD.MountClusterClientSecret != nil && *tc.Spec.PD.MountClusterClientSecret {
			vols = append(vols, corev1.Volume{
				Name: util.ClusterClientVolName, VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: util.ClusterClientTLSSecretName(tc.Name),
					},
				},
			})
		}
	}
	if tc.Spec.TiDB != nil && tc.Spec.TiDB.IsTLSClientEnabled() && !tc.SkipTLSWhenConnectTiDB() && clusterVersionGE4 {
		vols = append(vols, corev1.Volume{
			Name: "tidb-client-tls", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: util.TiDBClientTLSSecretName(tc.Name, tc.Spec.PD.TLSClientSecretName),
				},
			},
		})
	}
	// handle StorageVolumes and AdditionalVolumeMounts in ComponentSpec

	storageVolMounts, additionalPVCs := util.BuildStorageVolumeAndVolumeMount(tc.Spec.PD.StorageVolumes, tc.Spec.PD.StorageClassName, v1alpha1.PDMemberType)
	volMounts = append(volMounts, storageVolMounts...)
	volMounts = append(volMounts, tc.Spec.PD.AdditionalVolumeMounts...)

	sysctls := "sysctl -w"
	var initContainers []corev1.Container
	if basePDSpec.Annotations() != nil {
		init, ok := basePDSpec.Annotations()[label.AnnSysctlInit]
		if ok && (init == label.AnnSysctlInitVal) {
			if basePDSpec.PodSecurityContext() != nil && len(basePDSpec.PodSecurityContext().Sysctls) > 0 {
				for _, sysctl := range basePDSpec.PodSecurityContext().Sysctls {
					sysctls = sysctls + fmt.Sprintf(" %s=%s", sysctl.Name, sysctl.Value)
				}
				privileged := true
				initContainers = append(initContainers, corev1.Container{
					Name:  "init",
					Image: tc.HelperImage(),
					Command: []string{
						"sh",
						"-c",
						sysctls,
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					// Init container resourceRequirements should be equal to app container.
					// Scheduling is done based on effective requests/limits,
					// which means init containers can reserve resources for
					// initialization that are not used during the life of the Pod.
					// ref:https://kubernetes.io/docs/concepts/workloads/pods/init-containers/#resources
					Resources: controller.ContainerResource(tc.Spec.PD.ResourceRequirements),
				})
			}
		}
	}
	// Init container is only used for the case where allowed-unsafe-sysctls
	// cannot be enabled for kubelet, so clean the sysctl in statefulset
	// SecurityContext if init container is enabled
	podSecurityContext := basePDSpec.PodSecurityContext().DeepCopy()
	if len(initContainers) > 0 {
		podSecurityContext.Sysctls = []corev1.Sysctl{}
	}

	storageRequest, err := controller.ParseStorageRequest(tc.Spec.PD.Requests) //pd磁盘使用
	if err != nil {
		return nil, fmt.Errorf("cannot parse storage request for PD, tidbcluster %s/%s, error: %v", tc.Namespace, tc.Name, err)
	}

	setName := controller.PDMemberName(tcName)
	stsLabels := label.New().Instance(instanceName).PD()
	podLabels := util.CombineStringMap(stsLabels, basePDSpec.Labels())
	podAnnotations := util.CombineStringMap(basePDSpec.Annotations(), controller.AnnProm(v1alpha1.DefaultPDClientPort, "/metrics"))
	stsAnnotations := getStsAnnotations(tc.Annotations, label.PDLabelVal)

	deleteSlotsNumber, err := util.GetDeleteSlotsNumber(stsAnnotations)
	if err != nil {
		return nil, fmt.Errorf("get delete slots number of statefulset %s/%s failed, err:%v", ns, setName, err)
	}

	pdContainer := corev1.Container{
		Name:            v1alpha1.PDMemberType.String(),
		Image:           tc.PDImage(),
		ImagePullPolicy: basePDSpec.ImagePullPolicy(),
		Command:         []string{"/bin/sh", "/usr/local/bin/pd_start_script.sh"},
		Ports: []corev1.ContainerPort{
			{
				Name:          "server",
				ContainerPort: v1alpha1.DefaultPDPeerPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "client",
				ContainerPort: v1alpha1.DefaultPDClientPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: volMounts,
		Resources:    controller.ContainerResource(tc.Spec.PD.ResourceRequirements),
	}

	if tc.Spec.PD.ReadinessProbe != nil {
		pdContainer.ReadinessProbe = &corev1.Probe{
			ProbeHandler:        buildPDReadinessProbHandler(tc),
			InitialDelaySeconds: int32(10),
		}
	}

	if tc.Spec.PD.ReadinessProbe != nil {
		if tc.Spec.PD.ReadinessProbe.InitialDelaySeconds != nil {
			pdContainer.ReadinessProbe.InitialDelaySeconds = *tc.Spec.PD.ReadinessProbe.InitialDelaySeconds
		}
		if tc.Spec.PD.ReadinessProbe.PeriodSeconds != nil {
			pdContainer.ReadinessProbe.PeriodSeconds = *tc.Spec.PD.ReadinessProbe.PeriodSeconds
		}
	}

	env := []corev1.EnvVar{
		{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name:  "PEER_SERVICE_NAME",
			Value: controller.PDPeerMemberName(tcName),
		},
		{
			Name:  "SERVICE_NAME",
			Value: controller.PDMemberName(tcName),
		},
		{
			Name:  "SET_NAME",
			Value: setName,
		},
		{
			Name:  "TZ",
			Value: tc.Spec.Timezone,
		},
	}

	podSpec := basePDSpec.BuildPodSpec()
	if basePDSpec.HostNetwork() {
		env = append(env, corev1.EnvVar{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		})
	}
	pdContainer.Env = util.AppendEnv(env, basePDSpec.Env())
	pdContainer.EnvFrom = basePDSpec.EnvFrom()
	podSpec.Volumes = append(vols, basePDSpec.AdditionalVolumes()...)
	podSpec.Containers, err = MergePatchContainers([]corev1.Container{pdContainer}, basePDSpec.AdditionalContainers())
	if err != nil {
		return nil, fmt.Errorf("failed to merge containers spec for PD of [%s/%s], error: %v", tc.Namespace, tc.Name, err)
	}

	podSpec.ServiceAccountName = tc.Spec.PD.ServiceAccount
	if podSpec.ServiceAccountName == "" {
		podSpec.ServiceAccountName = tc.Spec.ServiceAccount
	}
	podSpec.SecurityContext = podSecurityContext
	podSpec.InitContainers = append(initContainers, basePDSpec.InitContainers()...)

	updateStrategy := apps.StatefulSetUpdateStrategy{}
	if tc.Status.PD.VolReplaceInProgress {
		updateStrategy.Type = apps.OnDeleteStatefulSetStrategyType
	} else if basePDSpec.StatefulSetUpdateStrategy() == apps.OnDeleteStatefulSetStrategyType {
		updateStrategy.Type = apps.OnDeleteStatefulSetStrategyType
	} else {
		updateStrategy.Type = apps.RollingUpdateStatefulSetStrategyType
		updateStrategy.RollingUpdate = &apps.RollingUpdateStatefulSetStrategy{
			Partition: pointer.Int32Ptr(tc.PDStsDesiredReplicas() + deleteSlotsNumber),
		}
	}

	//构建最终pd的sts出来
	pdSet := &apps.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            setName,
			Namespace:       ns,
			Labels:          stsLabels.Labels(),
			Annotations:     stsAnnotations,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Spec: apps.StatefulSetSpec{
			Replicas: pointer.Int32Ptr(tc.PDStsDesiredReplicas()),
			Selector: stsLabels.LabelSelector(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: podSpec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				util.VolumeClaimTemplate(storageRequest, dataVolumeName, tc.Spec.PD.StorageClassName),
			},
			ServiceName:         controller.PDPeerMemberName(tcName),
			PodManagementPolicy: basePDSpec.PodManagementPolicy(),
			UpdateStrategy:      updateStrategy,
		},
	}

	pdSet.Spec.VolumeClaimTemplates = append(pdSet.Spec.VolumeClaimTemplates, additionalPVCs...)
	return pdSet, nil
}

func getPDConfigMap(tc *v1alpha1.TidbCluster) (*corev1.ConfigMap, error) {
	// For backward compatibility, only sync tidb configmap when .tidb.config is non-nil
	if tc.Spec.PD.Config == nil {
		return nil, nil
	}
	config := tc.Spec.PD.Config.DeepCopy() // use copy to not update tc spec

	clusterVersionGE4, err := clusterVersionGreaterThanOrEqualTo4(tc.PDVersion(), tc.Spec.PD.Mode)
	if err != nil {
		klog.V(4).Infof("cluster version: %s is not semantic versioning compatible", tc.PDVersion())
	}

	// override CA if tls enabled
	if tc.IsTLSClusterEnabled() {
		config.Set("security.cacert-path", path.Join(pdClusterCertPath, tlsSecretRootCAKey))
		config.Set("security.cert-path", path.Join(pdClusterCertPath, corev1.TLSCertKey))
		config.Set("security.key-path", path.Join(pdClusterCertPath, corev1.TLSPrivateKeyKey))
	}
	// Versions below v4.0 do not support Dashboard
	if tc.Spec.TiDB != nil && tc.Spec.TiDB.IsTLSClientEnabled() && !tc.SkipTLSWhenConnectTiDB() && clusterVersionGE4 {
		if !tc.Spec.TiDB.TLSClient.SkipInternalClientCA {
			config.Set("dashboard.tidb-cacert-path", path.Join(tidbClientCertPath, tlsSecretRootCAKey))
		}
		config.Set("dashboard.tidb-cert-path", path.Join(tidbClientCertPath, corev1.TLSCertKey))
		config.Set("dashboard.tidb-key-path", path.Join(tidbClientCertPath, corev1.TLSPrivateKeyKey))
	}

	if tc.Spec.PD.EnableDashboardInternalProxy != nil {
		config.Set("dashboard.internal-proxy", *tc.Spec.PD.EnableDashboardInternalProxy)
	}

	confText, err := config.MarshalTOML()
	if err != nil {
		return nil, err
	}

	startScript, err := startscript.RenderPDStartScript(tc)
	if err != nil {
		return nil, err
	}

	instanceName := tc.GetInstanceName()
	pdLabel := label.New().Instance(instanceName).PD().Labels()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            controller.PDMemberName(tc.Name),
			Namespace:       tc.Namespace,
			Labels:          pdLabel,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Data: map[string]string{
			"config-file":    string(confText),
			"startup-script": startScript,
		},
	}
	return cm, nil
}

func clusterVersionGreaterThanOrEqualTo4(version, model string) (bool, error) {
	// TODO: remove `model` and `nightly` when support pd microservice docker
	if model == "ms" && version == "nightly" {
		return true, nil
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return true, err
	}

	return v.Major() >= 4, nil
}

// find PD pods in set that have not joined the PD cluster yet.
// pdStatus contains the PD members in the PD cluster.
func (m *pdMemberManager) collectUnjoinedMembers(tc *v1alpha1.TidbCluster, set *apps.StatefulSet, pdStatus map[string]v1alpha1.PDMember) error {
	ns := tc.GetNamespace()
	podSelector, podSelectErr := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if podSelectErr != nil {
		return podSelectErr
	}
	pods, podErr := m.deps.PodLister.Pods(tc.Namespace).List(podSelector)
	if podErr != nil {
		return fmt.Errorf("collectUnjoinedMembers: failed to list pods for cluster %s/%s, selector %s, error %v", tc.GetNamespace(), tc.GetName(), set.Spec.Selector, podErr)
	}

	// check all pods in PD sts to see whether it has already joined the PD cluster
	unjoined := map[string]v1alpha1.UnjoinedMember{}
	for _, pod := range pods {
		var joined = false
		// if current PD pod name is in the keys of pdStatus, it has joined the PD cluster
		for pdName := range pdStatus {
			ordinal, err := util.GetOrdinalFromPodName(pod.Name)
			if err != nil {
				return fmt.Errorf("unexpected pod name %q: %v", pod.Name, err)
			}
			if strings.EqualFold(PdName(tc.Name, ordinal, tc.Namespace, tc.Spec.ClusterDomain, tc.Spec.AcrossK8s), pdName) {
				joined = true
				break
			}
		}
		if !joined {
			pvcs, err := util.ResolvePVCFromPod(pod, m.deps.PVCLister)
			if err != nil {
				return fmt.Errorf("collectUnjoinedMembers: failed to get pvcs for pod %s/%s, error: %s", ns, pod.Name, err)
			}
			pvcUIDSet := make(map[types.UID]v1alpha1.EmptyStruct)
			for _, pvc := range pvcs {
				pvcUIDSet[pvc.UID] = v1alpha1.EmptyStruct{}
			}
			unjoined[pod.Name] = v1alpha1.UnjoinedMember{
				PodName:   pod.Name,
				PVCUIDSet: pvcUIDSet,
				CreatedAt: metav1.Now(),
			}
		}
	}

	tc.Status.PD.UnjoinedMembers = unjoined
	return nil
}

// TODO: Support check status http request in future.
func buildPDReadinessProbHandler(tc *v1alpha1.TidbCluster) corev1.ProbeHandler {
	return corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromInt(int(v1alpha1.DefaultPDClientPort)),
		},
	}
}

// TODO: seems not used
type FakePDMemberManager struct {
	err error
}

func NewFakePDMemberManager() *FakePDMemberManager {
	return &FakePDMemberManager{}
}

func (m *FakePDMemberManager) SetSyncError(err error) {
	m.err = err
}

// func (m *pdMemberManager) Sync(tc *v1alpha1.TidbCluster) error {
func (m *FakePDMemberManager) Sync(tc *v1alpha1.TidbCluster) error {
	if m.err != nil {
		return m.err
	}
	if len(tc.Status.PD.Members) != 0 {
		// simulate status update
		tc.Status.ClusterID = string(uuid.NewUUID())
	}
	return nil
}
