// Package node contains implementation of CSI Node component
package node

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	k8sError "k8s.io/apimachinery/pkg/api/errors"

	api "eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/generated/v1"
	apiV1 "eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/v1"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/base"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/base/util"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/common"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/sc"
)

// SCName type means that depending on SC and parameters in CreateVolumeRequest()
// here we should use different SC implementations for creating required volumes
// the same principle we can use in Controller Server or read from a CRD instance
// store storage class name
type SCName string

// CSINodeService is the implementation of NodeServer interface from GO CSI specification.
// Contains VolumeManager in a such way that it is a single instance in the driver
type CSINodeService struct {
	NodeID string
	log    *logrus.Entry
	svc    common.VolumeOperations
	reqMu  sync.Mutex
	VolumeManager
	grpc_health_v1.HealthServer
}

const (
	// PodNameKey to read pod name from PodInfoOnMount feature
	PodNameKey = "csi.storage.k8s.io/pod.name"
	// EphemeralKey in volume context means that in node publish request we need to create ephemeral volume
	EphemeralKey = "csi.storage.k8s.io/ephemeral"
)

// NewCSINodeService is the constructor for CSINodeService struct
// Receives an instance of HWServiceClient to interact with HWManager, ID of a node where it works, logrus logger
// and base.KubeClient
// Returns an instance of CSINodeService
func NewCSINodeService(client api.HWServiceClient, nodeID string, logger *logrus.Logger, k8sclient *base.KubeClient) *CSINodeService {
	s := &CSINodeService{
		VolumeManager: *NewVolumeManager(client, &base.Executor{}, logger, k8sclient, nodeID),
		NodeID:        nodeID,
		svc:           common.NewVolumeOperationsImpl(k8sclient, logger),
	}
	s.log = logger.WithField("component", "CSINodeService")
	return s
}

// NodeStageVolume is the implementation of CSI Spec NodeStageVolume. Performs when the first pod consumes a volume.
// This method mounts volume with appropriate VolumeID into the StagingTargetPath from request.
// Receives golang context and CSI Spec NodeStageVolumeRequest
// Returns CSI Spec NodeStageVolumeResponse or error if something went wrong
func (s *CSINodeService) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method":   "NodeStageVolume",
		"volumeID": req.GetVolumeId(),
	})

	ll.Infof("Processing request: %v", req)

	// Check arguments
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetStagingTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Stage Path missing in request")
	}

	volumeID := req.VolumeId
	v, ok := s.getFromVolumeCache(volumeID)
	if !ok {
		message := fmt.Sprintf("No volume with ID %s found on node", volumeID)
		ll.Error(message)
		return nil, status.Error(codes.NotFound, message)
	}

	if v.CSIStatus == apiV1.Failed {
		return nil, fmt.Errorf("corresponding volume CR %s reached failed status", v.Id)
	}

	scImpl := s.getStorageClassImpl(v.StorageClass)
	ll.Infof("Chosen StorageClass is %s", v.StorageClass)

	targetPath := req.StagingTargetPath

	partition, err := s.constructPartition(v)
	if err != nil {
		ll.Error("failed to get partition, error: ", err)
		return nil, status.Error(codes.Internal, "failed to publish volume")
	}

	if v.CSIStatus == apiV1.VolumeReady {
		ll.Info("Perform mount operation")
		if err := scImpl.Mount(partition, targetPath); err != nil {
			ll.Errorf("Failed to stage volume %s, error: %v", v.Id, err)
			return nil, fmt.Errorf("failed to stage volume %s", v.Id)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}
	ll.Infof("Work with partition %s", partition)

	if err := s.prepareAndPerformMount(partition, targetPath, scImpl, false); err != nil {
		s.setVolumeStatus(req.VolumeId, apiV1.Failed)
		return nil, fmt.Errorf("failed to stage volume")
	}
	s.setVolumeStatus(req.VolumeId, apiV1.VolumeReady)
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume is the implementation of CSI Spec NodeUnstageVolume. Performs when the last pod stops consume
// a volume. This method unmounts volume with appropriate VolumeID from the StagingTargetPath from request.
// Receives golang context and CSI Spec NodeUnstageVolumeRequest
// Returns CSI Spec NodeUnstageVolumeResponse or error if something went wrong
func (s *CSINodeService) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method":   "NodeUnstageVolume",
		"volumeID": req.GetVolumeId(),
	})

	ll.Infof("Processing request: %v", req)

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetStagingTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Stage Path missing in request")
	}
	v, ok := s.getFromVolumeCache(req.GetVolumeId())
	if !ok {
		return nil, status.Error(codes.Internal, "Unable to find volume")
	}

	if v.CSIStatus == apiV1.Failed {
		//Should we return error or nil?
		return nil, status.Errorf(codes.Internal, "corresponding CR %s reached Failed status", v.Id)
	}

	// This is a temporary solution to clear all owners during NodeUnstage
	// because NodeUnpublishRequest doesn't contain info about pod
	// TODO AK8S-466 Remove owner from Owners slice during Unpublish properly
	// Not fail NodeUnstageRequest if can't clear owners
	if err := s.clearVolumeOwners(req.GetVolumeId()); err != nil {
		ll.Errorf("Failed to clear owners: %v", err)
	}

	if err := s.unmount(v.StorageClass, req.GetStagingTargetPath()); err != nil {
		s.setVolumeStatus(v.Id, apiV1.Failed)
		return nil, err
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// unmount uses in Unstage/Unpublish requests to avoid duplicated code
func (s *CSINodeService) unmount(storageClass string, path string) error {
	ll := s.log.WithFields(logrus.Fields{
		"method": "unmount",
	})
	scImpl := s.getStorageClassImpl(storageClass)

	err := scImpl.Unmount(path)
	if err != nil {
		return status.Error(codes.Internal, "Unable to unmount")
	}

	ll.Infof("volume was successfully unmount from %s", path)
	return nil
}

// prepareAndPerformMount is used it in Stage/Publish requests to prepareAndPerformMount scrPath to targetPath, opts are used for prepareAndPerformMount commands
func (s *CSINodeService) prepareAndPerformMount(srcPath, targetPath string, scImpl sc.StorageClassImplementer, bind bool) error {
	ll := s.log.WithFields(logrus.Fields{
		"method": "prepareAndPerformMount",
	})
	if err := scImpl.CreateTargetPath(targetPath); err != nil {
		return err
	}
	mounted, err := scImpl.IsMountPoint(targetPath)
	if err != nil {
		ll.Info("IsMountPoint failed: ", err)
		_ = scImpl.DeleteTargetPath(targetPath)
		return err
	}
	if mounted {
		ll.Infof("Mount point already exist")
		return nil
	}
	var opts string
	if bind {
		opts = "--bind"
	}
	if err := scImpl.Mount(srcPath, targetPath, opts); err != nil {
		_ = scImpl.DeleteTargetPath(targetPath)
		return err
	}
	return nil
}

// NodePublishVolume is the implementation of CSI Spec NodePublishVolume. Performs each time pod starts consume
// a volume. This method perform bind mount of volume with appropriate VolumeID from the StagingTargetPath to TargetPath.
// Receives golang context and CSI Spec NodePublishVolumeRequest
// Returns CSI Spec NodePublishVolumeResponse or error if something went wrong
func (s *CSINodeService) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method":   "NodePublishVolume",
		"volumeID": req.GetVolumeId(),
	})

	ll.Infof("Processing request: %v", req)

	// Check arguments
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target Path missing in request")
	}
	var (
		inline bool
		err    error
	)
	srcPath := req.GetStagingTargetPath()
	path := req.GetTargetPath()
	if req.GetVolumeContext() != nil {
		val, ok := req.GetVolumeContext()[EphemeralKey]
		if ok {
			inline, err = strconv.ParseBool(val)
			if err != nil {
				ll.Errorf("Failed to parse value %v to bool", val)
			}
		}
	}

	//For prepareAndPerformMount function
	bind := true
	volumeID := req.GetVolumeId()
	//Inline volume has the same cycle as usual volume, but k8s calls only Publish/Unpulish methods so we need to call CreateVolume before publish it
	if inline {
		vol, err := s.createInlineVolume(ctx, volumeID, req)
		if err != nil {
			ll.Error("failed to create inline volume, error: ", err)
			return nil, status.Error(codes.Internal, "failed to publish volume")
		}
		srcPath, err = s.constructPartition(vol)
		if err != nil {
			ll.Error("failed to get partition, error: ", err)
			return nil, status.Error(codes.Internal, "failed to publish volume")
		}
		//For inline volume mount is performed without options
		bind = false
	} else if len(srcPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging Path missing in request")
	}
	v, ok := s.getFromVolumeCache(volumeID)
	if !ok {
		return nil, status.Error(codes.Internal, "Unable to find volume")
	}
	// Not fail NodePublishRequest if can't set owner
	podName, ok := req.VolumeContext[PodNameKey]
	if !ok {
		ll.Infof("podInfoOnMound flag is not provided")
	} else if err := s.addVolumeOwner(volumeID, podName); err != nil {
		ll.Errorf("Failed to set owner %s: %v", podName, err)
	}
	if v.CSIStatus == apiV1.Failed {
		return nil, fmt.Errorf("corresponding volume CR %s reached failed status", v.Id)
	}
	scImpl := s.getStorageClassImpl(v.StorageClass)

	if err := s.prepareAndPerformMount(srcPath, path, scImpl, bind); err != nil {
		ll.Errorf("prepareAndPerformMount failed, set status to %s", apiV1.Failed)
		s.setVolumeStatus(v.Id, apiV1.Failed)
		return nil, fmt.Errorf("failed to publish volume")
	}
	ll.Infof("Set status to %s", apiV1.Published)
	s.setVolumeStatus(v.Id, apiV1.Published)
	return &csi.NodePublishVolumeResponse{}, nil
}

//createInlineVolume encapsulate logic for creating inline volumes
func (s *CSINodeService) createInlineVolume(ctx context.Context, volumeID string, req *csi.NodePublishVolumeRequest) (*api.Volume, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method":   "createInlineVolume",
		"volumeID": volumeID,
	})

	var (
		volumeContext = req.GetVolumeContext() // verified in NodePublishVolume method
		bytesStr      = volumeContext[base.SizeKey]
		fsType        = "None"
		mode          string
		sc            string
		bytes         int64
		err           error
	)

	if bytes, err = base.StrToBytes(bytesStr); err != nil {
		ll.Errorf("Failed to parse value %v to bytes", bytesStr)
		return nil, err
	}

	if accessType, ok := req.GetVolumeCapability().AccessType.(*csi.VolumeCapability_Mount); ok {
		fsType = strings.ToLower(accessType.Mount.FsType)
		if fsType == "" {
			fsType = base.DefaultFsType
			ll.Infof("FS type wasn't provide. Will use %s as a default value", fsType)
		}
		mode = apiV1.ModeFS
	}

	sc = base.ConvertStorageClass(volumeContext[base.StorageTypeKey])
	if sc == apiV1.StorageClassAny {
		sc = apiV1.StorageClassHDD // do not use sc ANY for inline volumes
	}

	s.reqMu.Lock()
	vol, err := s.svc.CreateVolume(ctx, api.Volume{
		Id:           volumeID,
		StorageClass: sc,
		NodeId:       s.NodeID,
		Size:         bytes,
		Ephemeral:    true,
		Mode:         mode,
		Type:         fsType,
	})
	s.reqMu.Unlock()
	if err != nil {
		return nil, err
	}

	if vol.CSIStatus == apiV1.Creating {
		if err = s.svc.WaitStatus(ctx, vol.Id, apiV1.Failed, apiV1.Created); err != nil {
			return nil, err
		}
	}

	if err = s.acProvider.DeleteIfEmpty(ctx, vol.Location); err != nil {
		ll.Errorf("Unable to check AC size by location: %v", err)
	}

	return vol, nil
}

//constructPartition tries to find partition name for particular Volume. It searches drive path and serial number by volume Location,
//then GetPartitionNameByUUID is called for device and uuid to evaluate partition
func (s *CSINodeService) constructPartition(volume *api.Volume) (string, error) {
	var partition string
	switch volume.StorageClass {
	case apiV1.StorageClassHDDLVG, apiV1.StorageClassSSDLVG:
		vgName := volume.Location
		var err error

		// for LVG based on system disk LVG CR name != VG name
		// need to read appropriate LVG CR and use LVG CR.Spec.Name as VG name
		if volume.StorageClass == apiV1.StorageClassSSDLVG {
			vgName, err = s.k8sclient.GetVGNameByLVGCRName(context.Background(), volume.Location)
			if err != nil {
				return "", err
			}
		}

		partition = fmt.Sprintf("/dev/%s/%s", vgName, volume.Id)
	default:
		//TODO AK8S-380 Make drives cache thread safe
		s.dCacheMu.Lock()
		drive := s.drivesCache[volume.Location]
		s.dCacheMu.Unlock()
		if drive == nil {
			return "", fmt.Errorf("drive with uuid %s wasn't found ", volume.Location)
		}

		// get device path
		bdev, err := s.linuxUtils.SearchDrivePath(drive)
		if err != nil {
			return "", status.Errorf(codes.Internal, "unable to find device for drive with S/N %s", volume.Location)
		}
		uuid, _ := util.GetVolumeUUID(volume.Id)
		//TODO temporary solution because of ephemeral volumes volume id https://jira.cec.lab.emc.com:8443/browse/AK8S-749
		if volume.Ephemeral {
			uuid, err = s.linuxUtils.GetPartitionUUID(bdev)
			if err != nil {
				return "", status.Errorf(codes.Internal, "unable to find partition by device %s", bdev)
			}
			time.Sleep(SleepBetweenRetriesToSyncPartTable)
		}
		// get partition name
		partition, err = s.linuxUtils.GetPartitionNameByUUID(bdev, uuid)
		if err != nil {
			return "", err
		}
	}
	return partition, nil
}

// NodeUnpublishVolume is the implementation of CSI Spec NodePublishVolume. Performs each time pod stops consume a volume.
// This method unmounts volume with appropriate VolumeID from the TargetPath.
// Receives golang context and CSI Spec NodeUnpublishVolumeRequest
// Returns CSI Spec NodeUnpublishVolumeResponse or error if something went wrong
func (s *CSINodeService) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method":   "NodeUnpublishVolume",
		"volumeID": req.GetVolumeId(),
	})

	ll.Infof("Processing request: %v", req)

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target Path missing in request")
	}

	v, ok := s.getFromVolumeCache(req.GetVolumeId())
	if !ok {
		return nil, status.Error(codes.NotFound, "Unable to find volume")
	}
	if err := s.unmount(v.StorageClass, req.GetTargetPath()); err != nil {
		s.setVolumeStatus(v.Id, apiV1.Failed)
		return nil, err
	}
	//If volume has more than 1 owner pods then keep its status as Published
	if len(v.Owners) > 1 {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	s.setVolumeStatus(v.Id, apiV1.VolumeReady)
	volume, ok := s.getFromVolumeCache(req.GetVolumeId())
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "Volume not found")
	}
	//k8s dosn't call DeleteVolume for inline volumes, so we perform DeleteVolume operation in Unpublish request
	if volume.Ephemeral {
		s.reqMu.Lock()
		err := s.svc.DeleteVolume(ctx, req.GetVolumeId())
		s.reqMu.Unlock()
		if err != nil {
			if k8sError.IsNotFound(err) {
				ll.Infof("Volume doesn't exist")
				return &csi.NodeUnpublishVolumeResponse{}, nil
			}
			ll.Errorf("Unable to delete volume: %v", err)
			return nil, err
		}

		if err = s.svc.WaitStatus(ctx, req.VolumeId, apiV1.Failed, apiV1.Removed); err != nil {
			return nil, status.Error(codes.Internal, "Unable to delete volume")
		}
		s.reqMu.Lock()
		s.svc.UpdateCRsAfterVolumeDeletion(ctx, req.VolumeId)
		s.reqMu.Unlock()
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats returns empty response
func (s *CSINodeService) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return &csi.NodeGetVolumeStatsResponse{}, nil
}

// NodeExpandVolume returns empty response
func (s *CSINodeService) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetCapabilities is the implementation of CSI Spec NodeGetCapabilities.
// Provides Node capabilities of CSI driver to k8s. STAGE/UNSTAGE Volume for now.
// Receives golang context and CSI Spec NodeGetCapabilitiesRequest
// Returns CSI Spec NodeGetCapabilitiesResponse and nil error
func (s *CSINodeService) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{Capabilities: []*csi.NodeServiceCapability{
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
		}},
	}, nil
}

// NodeGetInfo is the implementation of CSI Spec NodeGetInfo. It plays a role in CSI Topology feature when Controller
// chooses a node where to deploy a volume.
// Receives golang context and CSI Spec NodeGetInfoRequest
// Returns CSI Spec NodeGetInfoResponse with topology "baremetal-csi/nodeid": NodeID and nil error
func (s *CSINodeService) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method": "NodeGetInfo",
	})

	topology := csi.Topology{
		Segments: map[string]string{
			"baremetal-csi/nodeid": s.NodeID,
		},
	}

	ll.Infof("NodeGetInfo created topology: %v", topology)

	return &csi.NodeGetInfoResponse{
		NodeId:             s.NodeID,
		AccessibleTopology: &topology,
	}, nil
}

// Check does the health check and changes the status of the server based on drives cache size
//TODO AK8S-379 Investigate readiness and liveness probes conditions
func (s *CSINodeService) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	ll := s.log.WithFields(logrus.Fields{
		"method": "Check",
	})

	if !s.initialized {
		ll.Info("Node svc is not ready yet")
		return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING}, nil
	}

	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// Watch is used by clients to receive updates when the svc status changes.
// Watch only dummy implemented just to satisfy the interface.
func (s *CSINodeService) Watch(req *grpc_health_v1.HealthCheckRequest, srv grpc_health_v1.Health_WatchServer) error {
	return status.Errorf(codes.Unimplemented, "method Watch not implemented")
}
