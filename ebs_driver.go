package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
)

type ebsVolumeDriver struct {
	ec2                 *ec2.EC2
	ec2meta             *ec2metadata.EC2Metadata
	awsInstanceId       string
	awsRegion           string
	awsAvailabilityZone string
}

func NewEbsVolumeDriver() (VolumeDriver, error) {
	d := &ebsVolumeDriver{}

	ec2sess := session.New()
	d.ec2meta = ec2metadata.New(ec2sess)

	// Fetch AWS information, validating along the way.
	if !d.ec2meta.Available() {
		return nil, errors.New("Not running on an EC2 instance.")
	}
	var err error
	if d.awsInstanceId, err = d.ec2meta.GetMetadata("instance-id"); err != nil {
		return nil, err
	}
	if d.awsRegion, err = d.ec2meta.Region(); err != nil {
		return nil, err
	}
	if d.awsAvailabilityZone, err =
		d.ec2meta.GetMetadata("placement/availability-zone"); err != nil {
		return nil, err
	}

	d.ec2 = ec2.New(ec2sess, &aws.Config{Region: aws.String(d.awsRegion)})

	// Print some diagnostic information and then return the driver.
	log("Auto-detected EC2 information:\n")
	log("\tInstanceId        : %v\n", d.awsInstanceId)
	log("\tRegion            : %v\n", d.awsRegion)
	log("\tAvailability Zone : %v\n", d.awsAvailabilityZone)
	return d, nil
}

func (d *ebsVolumeDriver) Create(path string) error {
	return nil
}

func (d *ebsVolumeDriver) Mount(path string) (string, error) {
	volume, folder := parsePath(path)
	mnt, err := d.doMount(volume)
	if err != nil {
		return "", err
	}
	return mnt + folder, nil
}

func (d *ebsVolumeDriver) Path(path string) (string, error) {
	volume, folder := parsePath(path)
	mnt := fmt.Sprintf("/mnt/blocker/%s%s", volume, folder)
	if stat, err := os.Stat(mnt); err != nil || !stat.IsDir() {
		return "", errors.New("Volume not mounted.")
	}
	return mnt, nil
}

func (d *ebsVolumeDriver) Remove(path string) error {
	volume, _ := parsePath(path)
	err := d.doUnmount(volume)
	if err != nil {
		return err
	}
	return nil
}

func (d *ebsVolumeDriver) Unmount(path string) error {
	volume, _ := parsePath(path)
	err := d.doUnmount(volume)
	if err != nil {
		return err
	}
	return nil
}

func parsePath(path string) (string, string) {
	sep := strings.Index(path, "/")
	if sep < 0 {
		return path, ""
	}
	return path[:sep], path[sep:]
}

func (d *ebsVolumeDriver) doMount(name string) (string, error) {
	// Auto-generate a random mountpoint.
	mnt := "/mnt/blocker/" + name

	// Ensure the directory /mnt/blocker/<m> exists.
	if err := os.MkdirAll(mnt, os.ModeDir|0700); err != nil {
		return "", err
	}
	if stat, err := os.Stat(mnt); err != nil || !stat.IsDir() {
		return "", fmt.Errorf("Mountpoint %v is not a directory: %v", mnt, err)
	}

	if err := exec.Command("mountpoint", "-q", mnt).Run(); err == nil {
		return mnt, nil
	}

	// Attach the EBS device to the current EC2 instance.
	dev, err := d.attachVolume(name)
	if err != nil {
		return "", err
	}

	// Now go ahead and mount the EBS device to the desired mountpoint.
	// TODO: support encrypted filesystems.
	if out, err := exec.Command("mount", dev, mnt).CombinedOutput(); err != nil {
		// Make sure to detach the instance before quitting (ignoring errors).
		d.detachVolume(name)

		return "", fmt.Errorf("Mounting device %v to %v failed: %v\n%v",
			dev, mnt, err, string(out))
	}

	// And finally set and return it.
	return mnt, nil
}

func (d *ebsVolumeDriver) waitUntilState(
	name string, check func(*ec2.Volume) error) error {
	// Most volume operations are asynchronous, and we often need to wait until
	// state transitions finish before proceeding to the mount.  Sadly, this
	// requires some clunky retries, sleeps, and that kind of crap.
	tries := 0
	for {
		tries++

		volumes, err := d.ec2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{aws.String(name)},
		})
		if err != nil {
			return err
		}

		// Check to see if the volume reached the intended state; if yes, return.
		err = check(volumes.Volumes[0])
		if err == nil {
			return nil
		}
		if tries == 12 {
			return err
		}

		log("\tWaiting for EBS attach to complete...\n")
		time.Sleep(5 * time.Second)
	}

	return nil
}

func (d *ebsVolumeDriver) waitUntilAttached(name string) error {
	return d.waitUntilState(name, func(volume *ec2.Volume) error {
		var attachment *ec2.VolumeAttachment
		if len(volume.Attachments) == 1 {
			attachment = volume.Attachments[0]
			if *attachment.State == ec2.VolumeAttachmentStateAttached {
				return nil
			}
		}
		if attachment == nil {
			return fmt.Errorf(
				"Volume state transition failed: expected 1 attachment, got %v",
				len(volume.Attachments))
		} else {
			return fmt.Errorf(
				"Volume state transition failed: seeking %v, current is %v",
				ec2.VolumeAttachmentStateAttached, *attachment.State)
		}
	})
}

func (d *ebsVolumeDriver) waitUntilAvailable(name string) error {
	return d.waitUntilState(name, func(volume *ec2.Volume) error {
		if *volume.State == ec2.VolumeStateAvailable {
			return nil
		}
		return fmt.Errorf(
			"Volume state transition failed: seeking %v, current is %v",
			ec2.VolumeStateAvailable, *volume.State)
	})
}

func (d *ebsVolumeDriver) attachVolume(name string) (string, error) {
	// Check if the volume is already attached to instance
	info, err := d.ec2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(name)},
	})
	if err != nil {
		return "", err
	}
	if len(info.Volumes[0].Attachments) == 1 {
		if *info.Volumes[0].Attachments[0].State == ec2.VolumeAttachmentStateAttached &&
			*info.Volumes[0].Attachments[0].InstanceId == d.awsInstanceId {
			re := regexp.MustCompile("/dev/(xv|s)d([f-p])")
			res := re.FindStringSubmatch(*info.Volumes[0].Attachments[0].Device)
			if len(res) != 3 {
				return "", errors.New("Unable to find mount device for " + name)
			}
			if _, err := os.Lstat("/dev/sd" + res[2]); err == nil {
				return "/dev/sd" + res[2], nil
			}
			if _, err := os.Lstat("/dev/xvd" + res[2]); err == nil {
				return "/dev/xvd" + res[2], nil
			}
		}
	}

	// Since detaching is asynchronous, we want to check first to see if the
	// target volume is in the process of being detached.  If it is, we'll wait
	// a little bit until it's ready to use.
	err = d.waitUntilAvailable(name)
	if err != nil {
		return "", err
	}

	// Now find the first free device to attach the EBS volume to.  See
	// http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/device_naming.html
	// for recommended naming scheme (/dev/sd[f-p]).
	for _, c := range "fghijklmnop" {
		dev := "/dev/sd" + string(c)
		altdev := "/dev/xvd" + string(c)

		if _, err := os.Lstat(dev); err == nil {
			continue
		}
		if _, err := os.Lstat(altdev); err == nil {
			continue
		}

		if _, err := d.ec2.AttachVolume(&ec2.AttachVolumeInput{
			Device:     aws.String(dev),
			InstanceId: aws.String(d.awsInstanceId),
			VolumeId:   aws.String(name),
		}); err != nil {
			if awsErr, ok := err.(awserr.Error); ok &&
				awsErr.Code() == "InvalidParameterValue" {
				// If AWS is simply reporting that the device is already in
				// use, then go ahead and check the next one.
				continue
			}

			return "", err
		}

		err = d.waitUntilAttached(name)
		if err != nil {
			return "", err
		}

		// Finally, the attach is complete.
		log("\tAttached EBS volume %v to %v:%v.\n", name, d.awsInstanceId, dev)
		if _, err := os.Lstat(dev); os.IsNotExist(err) {
			// On newer Linux kernels, /dev/sd* is mapped to /dev/xvd*.  See
			// if that's the case.
			if _, err := os.Lstat(altdev); os.IsNotExist(err) {
				d.detachVolume(name)
				return "", fmt.Errorf("Device %v is missing after attach.", dev)
			}

			log("\tLocal device name is %v\n", altdev)
			dev = altdev
		}

		return dev, nil
	}

	return "", errors.New("No devices available for attach: /dev/sd[f-p] taken.")
}

func (d *ebsVolumeDriver) doUnmount(name string) error {
	mnt := "/mnt/blocker/" + name

	// First unmount the device.
	if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("Unmounting %v failed: %v\n%v", mnt, err, string(out))
	}

	// Remove the mountpoint from the filesystem.
	if err := os.Remove(mnt); err != nil {
		return err
	}

	// Detach the EBS volume from this AWS instance.
	if err := d.detachVolume(name); err != nil {
		return err
	}

	// Finally clear out the slot and return.
	return nil
}

func (d *ebsVolumeDriver) detachVolume(name string) error {
	if _, err := d.ec2.DetachVolume(&ec2.DetachVolumeInput{
		InstanceId: aws.String(d.awsInstanceId),
		VolumeId:   aws.String(name),
	}); err != nil {
		return err
	}

	log("\tDetached EBS volume %v from %v.\n", name, d.awsInstanceId)
	return nil
}
