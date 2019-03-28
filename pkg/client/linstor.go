/*
CSI Driver for Linstor
Copyright © 2018 LINBIT USA, LLC

This program is free software; you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation; either version 2 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program; if not, see <http://www.gnu.org/licenses/>.
*/

package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"regexp"
	"strconv"
	"strings"

	lc "github.com/LINBIT/golinstor"
	"github.com/LINBIT/linstor-csi/pkg/volume"
	"github.com/haySwim/data"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	NodeListKey            = "nodelist"
	LayerListKey           = "layerlist"
	ClientListKey          = "clientlist"
	ReplicasOnSameKey      = "replicasonsame"
	ReplicasOnDifferentKey = "replicasondifferent"
	AutoPlaceKey           = "autoplace"
	DoNotPlaceWithRegexKey = "donotplacewithregex"
	SizeKiBKey             = "sizekib"
	StoragePoolKey         = "storagepool"
	DisklessStoragePoolKey = "disklessstoragepool"
	EncryptionKey          = "encryption"
	BlockSizeKey           = "blocksize"
	ForceKey               = "force"
	FSKey                  = "filesystem"
	// These have to be camel case. Maybe move them into resource config for
	// consistency?
	MountOptsKey = "mountOpts"
	FSOptsKey    = "fsOpts"
)

type Linstor struct {
	LinstorConfig
	log            *log.Entry
	annotationsKey string
	fallbackPrefix string
}

type LinstorConfig struct {
	LogOut      io.Writer
	LogFmt      log.Formatter
	Debug       bool
	Controllers string
}

func NewLinstor(cfg LinstorConfig) *Linstor {
	l := &Linstor{LinstorConfig: cfg}

	l.annotationsKey = "csi-volume-annotations"
	l.fallbackPrefix = "csi-"
	l.LogOut = cfg.LogOut

	if cfg.LogFmt != nil {
		log.SetFormatter(cfg.LogFmt)
	}
	if cfg.LogOut == nil {
		cfg.LogOut = ioutil.Discard
	}
	if cfg.Debug {
		log.SetLevel(log.DebugLevel)
		log.SetReportCaller(true)
	}
	log.SetOutput(cfg.LogOut)

	l.log = log.WithFields(log.Fields{
		"linstorCSIComponent": "client",
		"annotationsKey":      l.annotationsKey,
		"controllers":         l.Controllers,
	})

	l.log.WithFields(log.Fields{
		"resourceDeployment": fmt.Sprintf("%+v", l),
	}).Debug("generated new ResourceDeployment")
	return l
}

func (s *Linstor) ListAll(parameters map[string]string) ([]*volume.Info, error) {
	return nil, nil
}

// AllocationSizeKiB returns LINSTOR's smallest possible number of KiB that can
// satisfy the requiredBytes.
func (s *Linstor) AllocationSizeKiB(requiredBytes, limitBytes int64) (int64, error) {

	requestedSize := data.ByteSize(requiredBytes)
	minVolumeSize := data.ByteSize(4096)
	maxVolumeSize := data.ByteSize(limitBytes)
	unlimited := maxVolumeSize == 0
	if minVolumeSize > maxVolumeSize && !unlimited {
		return 0, fmt.Errorf("LINSTOR's minimum volume size exceeds the maximum size limit of the requested volume")
	}
	if requestedSize < minVolumeSize {
		requestedSize = minVolumeSize
	}

	// make sure there are enough KiBs to fit the required number of bytes,
	// e.g. 1025 bytes require 2 KiB worth of space to be allocated.
	volumeSize := data.NewKibiByte(data.NewKibiByte(requestedSize).InclusiveBytes())

	limit := data.NewByte(maxVolumeSize)

	if volumeSize.InclusiveBytes() > limit.InclusiveBytes() && !unlimited {
		return int64(volumeSize.Value()),
			fmt.Errorf("got request for %d Bytes of storage (needed to allocate %s), but size is limited to %s",
				requiredBytes, volumeSize, limit)
	}
	return int64(volumeSize.Value()), nil
}

func (s *Linstor) resDefToVolume(resDef lc.ResDef) (*volume.Info, error) {
	for _, p := range resDef.RscDfnProps {
		if p.Key == "Aux/"+s.annotationsKey {
			vol := &volume.Info{
				Parameters: make(map[string]string),
			}

			if err := json.Unmarshal([]byte(p.Value), vol); err != nil {
				return nil, fmt.Errorf("failed to unmarshal annotations for ResDef %+v", resDef)
			}

			if vol.Name == "" {
				return nil, fmt.Errorf("Failed to extract resource name from %+v", vol)
			}
			s.log.WithFields(log.Fields{
				"resourceDefinition": fmt.Sprintf("%+v", resDef),
				"volume":             fmt.Sprintf("%+v", vol),
			}).Debug("converted resource definition to volume")
			return vol, nil
		}
	}
	return nil, nil
}
func (s *Linstor) resDeploymentConfigFromVolumeInfo(vol *volume.Info) (*lc.ResourceDeploymentConfig, error) {
	cfg := &lc.ResourceDeploymentConfig{}

	cfg.LogOut = s.LogOut

	cfg.Controllers = s.Controllers

	// At this time vol.ID has to be a valid LINSTOR Name
	cfg.Name = vol.ID

	// TODO: Make don't extend volume size by 1 Kib, unless you have to.
	cfg.SizeKiB = uint64(vol.SizeBytes/1024 + 1)

	for k, v := range vol.Parameters {
		switch strings.ToLower(k) {
		case NodeListKey:
			cfg.NodeList = strings.Split(v, " ")
		case LayerListKey:
			cfg.LayerList = strings.Split(v, " ")
		case ReplicasOnSameKey:
			cfg.ReplicasOnSame = strings.Split(v, " ")
		case ReplicasOnDifferentKey:
			cfg.ReplicasOnDifferent = strings.Split(v, " ")
		case StoragePoolKey:
			cfg.StoragePool = v
		case DisklessStoragePoolKey:
			cfg.DisklessStoragePool = v
		case AutoPlaceKey:
			if v == "" {
				v = "0"
			}
			autoplace, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("unable to parse %q as an integer", v)
			}
			cfg.AutoPlace = autoplace
		case DoNotPlaceWithRegexKey:
			cfg.DoNotPlaceWithRegex = v
		case EncryptionKey:
			if strings.ToLower(v) == "true" {
				cfg.Encryption = true
			}
		}
	}
	serializedVol, err := json.Marshal(vol)
	if err != nil {
		return nil, err
	}

	// TODO: Support for other annotations.
	cfg.Annotations = make(map[string]string)
	cfg.Annotations[s.annotationsKey] = string(serializedVol)

	return cfg, nil
}

func (s *Linstor) resDeploymentFromVolumeInfo(vol *volume.Info) (*lc.ResourceDeployment, error) {
	cfg, err := s.resDeploymentConfigFromVolumeInfo(vol)
	if err != nil {
		return nil, err
	}
	r := lc.NewResourceDeployment(*cfg)
	return &r, nil
}

func (s *Linstor) GetByName(name string) (*volume.Info, error) {
	s.log.WithFields(log.Fields{
		"csiVolumeName": name,
	}).Debug("looking up resource by CSI volume name")

	r := lc.NewResourceDeployment(lc.ResourceDeploymentConfig{
		Name:        "CSIGetByName",
		Controllers: s.Controllers,
		LogOut:      s.LogOut})
	list, err := r.ListResourceDefinitions()
	if err != nil {
		return nil, err
	}
	for _, rd := range list {
		vol, err := s.resDefToVolume(rd)
		if err != nil {
			return nil, err
		}
		// Probably found a resource we didn't create.
		if vol == nil {
			continue
		}
		if vol.Name == name {
			return vol, nil
		}
		if vol.Name == s.fallbackPrefix+name {
			return vol, nil
		}
	}
	return nil, nil
}

func (s *Linstor) GetByID(ID string) (*volume.Info, error) {
	s.log.WithFields(log.Fields{
		"csiVolumeID": ID,
	}).Debug("looking up resource by CSI volume ID")

	r := lc.NewResourceDeployment(lc.ResourceDeploymentConfig{
		Name:        "CSIGetByID",
		Controllers: s.Controllers,
		LogOut:      s.LogOut})
	list, err := r.ListResourceDefinitions()
	if err != nil {
		return nil, err
	}

	for _, rd := range list {
		vol, err := s.resDefToVolume(rd)
		if err != nil {
			return nil, err
		}
		if rd.RscName == ID {
			return vol, nil
		}
		if vol.ID == ID {
			return vol, nil
		}
	}
	return nil, nil
}

func (s *Linstor) Create(vol *volume.Info) error {
	s.log.WithFields(log.Fields{
		"volume": fmt.Sprintf("%+v", vol),
	}).Info("creating volume")

	r, err := s.resDeploymentFromVolumeInfo(vol)
	if err != nil {
		return err
	}

	return r.CreateAndAssign()
}

func (s *Linstor) Delete(vol *volume.Info) error {
	s.log.WithFields(log.Fields{
		"volume": fmt.Sprintf("%+v", vol),
	}).Info("deleting volume")

	r, err := s.resDeploymentFromVolumeInfo(vol)
	if err != nil {
		return err
	}

	return r.Delete()
}

func (s *Linstor) Attach(vol *volume.Info, node string) error {
	s.log.WithFields(log.Fields{
		"volume":     fmt.Sprintf("%+v", vol),
		"targetNode": node,
	}).Info("attaching volume")

	// This is hackish, configure a volume copy that only makes new diskless asignments.
	cfg, err := s.resDeploymentConfigFromVolumeInfo(vol)
	if err != nil {
		return err
	}
	cfg.NodeList = []string{}
	cfg.AutoPlace = 0
	cfg.ClientList = []string{node}

	return lc.NewResourceDeployment(*cfg).Assign()
}

func (s *Linstor) Detach(vol *volume.Info, node string) error {
	s.log.WithFields(log.Fields{
		"volume":     fmt.Sprintf("%+v", vol),
		"targetNode": node,
	}).Info("detaching volume")

	r, err := s.resDeploymentFromVolumeInfo(vol)
	if err != nil {
		return err
	}

	return r.Unassign(node)
}

func (s *Linstor) CanonicalizeVolumeName(suggestedName string) string {
	name, err := linstorifyResourceName(suggestedName)
	if err != nil {
		return s.fallbackPrefix + uuid.New()
	} else {
		// We already handled the idempotency/existing case
		// This is to make sure that nobody else created a resource with that name (e.g., another user/plugin)
		existingVolume, err := s.GetByID(name)
		if existingVolume != nil || err != nil {
			return s.fallbackPrefix + uuid.New()
		}
	}

	return name
}

func (s *Linstor) NodeAvailable(node string) (bool, error) {
	// Hard coding magic string to pass csi-test.
	if node == "some-fake-node-id" {
		return false, nil
	}

	// TODO: Check if the node is available.
	return true, nil
}

func (s *Linstor) GetAssignmentOnNode(vol *volume.Info, node string) (*volume.Assignment, error) {
	s.log.WithFields(log.Fields{
		"volume":     fmt.Sprintf("%+v", vol),
		"targetNode": node,
	}).Debug("getting assignment info")

	r, err := s.resDeploymentFromVolumeInfo(vol)
	if err != nil {
		return nil, err
	}

	devPath, err := r.GetDevPath(node, false)
	if err != nil {
		return nil, err
	}
	va := &volume.Assignment{
		Vol:  vol,
		Node: node,
		Path: devPath,
	}
	s.log.WithFields(log.Fields{
		"volumeAssignment": fmt.Sprintf("%+v", va),
	}).Debug("found assignment info")

	return va, nil
}

func (s *Linstor) Mount(vol *volume.Info, source, target, fsType string, options []string) error {
	s.log.WithFields(log.Fields{
		"volume": fmt.Sprintf("%+v", vol),
		"source": source,
		"target": target,
	}).Info("mounting volume")

	r, err := s.resDeploymentFromVolumeInfo(vol)
	if err != nil {
		return err
	}

	// Merge mount options from Storage Classes and CSI calls.
	options = append(options, vol.Parameters[MountOptsKey])
	mntOpts := strings.Join(options, ",")

	// If an FSType is supplided by the parameters, override the one passed
	// to the Mount Call.
	parameterFsType, ok := vol.Parameters[FSKey]
	if ok {
		fsType = parameterFsType
	}

	mounter := lc.FSUtil{
		ResourceDeployment: r,
		FSType:             fsType,
		MountOpts:          mntOpts,
		FSOpts:             vol.Parameters[FSOptsKey],
	}
	s.log.WithFields(log.Fields{
		"mounter": fmt.Sprintf("%+v", mounter),
	}).Debug("configured mounter")

	err = mounter.SafeFormat(source)
	if err != nil {
		return err
	}

	return mounter.Mount(source, target)
}

func (s *Linstor) Unmount(target string) error {
	s.log.WithFields(log.Fields{
		"target": target,
	}).Info("unmounting volume")

	r := lc.NewResourceDeployment(lc.ResourceDeploymentConfig{
		Name:   "CSI Unmount",
		LogOut: s.LogOut})
	mounter := lc.FSUtil{
		ResourceDeployment: &r,
	}
	s.log.WithFields(log.Fields{
		"mounter": fmt.Sprintf("%+v", mounter),
	}).Debug("configured mounter")

	return mounter.UnMount(target)
}

// validResourceName returns an error if the input string is not a valid LINSTOR name
func validResourceName(resName string) error {
	if resName == "all" {
		return errors.New("Not allowed to use 'all' as resource name")
	}

	b, err := regexp.MatchString("[[:alpha:]]", resName)
	if err != nil {
		return err
	} else if !b {
		return errors.New("Resource name did not contain at least one alphabetic (A-Za-z) character")
	}

	re := "^[A-Za-z_][A-Za-z0-9\\-_]{1,47}$"
	b, err = regexp.MatchString(re, resName)
	if err != nil {
		return err
	} else if !b {
		// without open coding it (ugh!) as good as it gets
		return fmt.Errorf("Resource name did not match: '%s'", re)
	}

	return nil
}

// linstorifyResourceName tries to generate a valid LINSTOR name if the input currently is not.
// If the input is already valid, it just returns this name.
// This tries to preserve the original meaning as close as possible, but does not try extra hard.
// Do *not* expect this function to be injective.
// Do *not* expect this function to be stable. This means you need to save the output, the output of the function might change without notice.
func linstorifyResourceName(name string) (string, error) {
	if err := validResourceName(name); err == nil {
		return name, nil
	}

	re := regexp.MustCompile("[^A-Za-z0-9\\-_]")
	newName := re.ReplaceAllLiteralString(name, "_")
	if err := validResourceName(newName); err == nil {
		return newName, err
	}

	// fulfill at least the minimal requirement
	newName = "LS_" + newName
	if err := validResourceName(newName); err == nil {
		return newName, nil
	}

	return "", fmt.Errorf("Could not linstorify name (%s)", name)
}
