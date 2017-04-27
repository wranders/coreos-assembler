// Copyright 2016 CoreOS, Inc.
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

package aws

import (
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
)

func (a *API) AddKey(name, key string) error {
	_, err := a.ec2.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           &name,
		PublicKeyMaterial: []byte(key),
	})

	return err
}

func (a *API) DeleteKey(name string) error {
	_, err := a.ec2.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: &name,
	})

	return err
}

// CheckInstances waits until a set of EC2 instances are accessible by SSH, waiting a maximum of 'd' time.
func (a *API) CheckInstances(ids []string, d time.Duration) error {
	after := time.After(d)
	online := make(map[string]bool)

	// loop until all machines are online
	for len(ids) != len(online) {
		select {
		case <-after:
			return fmt.Errorf("timed out waiting for instances to run")
		default:
		}

		// don't make api calls too quickly, or we will hit the rate limit
		time.Sleep(10 * time.Second)

		getinst := &ec2.DescribeInstancesInput{
			InstanceIds: aws.StringSlice(ids),
		}

		insts, err := a.ec2.DescribeInstances(getinst)
		if err != nil {
			return err
		}

		for _, r := range insts.Reservations {
			for _, i := range r.Instances {
				switch *i.State.Name {
				case ec2.InstanceStateNamePending:
					// continue
				case ec2.InstanceStateNameRunning:
					// skip instances known to be up
					if online[*i.InstanceId] {
						continue
					}

					if i.PublicIpAddress == nil {
						continue
					}

					// XXX: ssh is a terrible way to check this, but it is all we have.
					c, err := net.DialTimeout("tcp", *i.PublicIpAddress+":22", 3*time.Second)
					if err != nil {
						continue
					}
					c.Close()

					online[*i.InstanceId] = true
				default:
					// instances should not be stopping, shutting-down, terminated, etc.
					return fmt.Errorf("instance %v in unexpected state %q", *i.InstanceId, *i.State.Name)
				}
			}
		}
	}

	return nil
}

// CreateInstances creates EC2 instances with a given name tag, ssh key name, user data. The image ID, instance type, and security group set in the API will be used. If wait is true, CreateInstances will block until all instances are reachable by SSH.
func (a *API) CreateInstances(name, keyname, userdata string, count uint64, wait bool) ([]*ec2.Instance, error) {
	cnt := int64(count)

	var ud *string
	if len(userdata) > 0 {
		tud := base64.StdEncoding.EncodeToString([]byte(userdata))
		ud = &tud
	}

	inst := ec2.RunInstancesInput{
		ImageId:        &a.opts.AMI,
		MinCount:       &cnt,
		MaxCount:       &cnt,
		KeyName:        &keyname,
		InstanceType:   &a.opts.InstanceType,
		SecurityGroups: []*string{&a.opts.SecurityGroup},
		UserData:       ud,
	}

	reservations, err := a.ec2.RunInstances(&inst)
	if err != nil {
		return nil, err
	}

	ids := make([]string, len(reservations.Instances))
	for i, inst := range reservations.Instances {
		ids[i] = *inst.InstanceId
	}

	for {
		_, err := a.ec2.CreateTags(&ec2.CreateTagsInput{
			Resources: aws.StringSlice(ids),
			Tags: []*ec2.Tag{
				&ec2.Tag{
					Key:   aws.String("Name"),
					Value: aws.String(name),
				},
			},
		})
		if err == nil {
			break
		}
		if awserr, ok := err.(awserr.Error); !ok || awserr.Code() != "InvalidInstanceID.NotFound" {
			a.TerminateInstances(ids)
			return nil, fmt.Errorf("error creating tags: %v", err)
		}
		// eventual consistency
		time.Sleep(5 * time.Second)
	}

	if !wait {
		return reservations.Instances, nil
	}

	// 5 minutes is a pretty reasonable timeframe for AWS instances to work.
	if err := a.CheckInstances(ids, 10*time.Minute); err != nil {
		a.TerminateInstances(ids)
		return nil, err
	}

	// call DescribeInstances to get machine IP
	getinst := &ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice(ids),
	}

	insts, err := a.ec2.DescribeInstances(getinst)
	if err != nil {
		a.TerminateInstances(ids)
		return nil, err
	}

	return insts.Reservations[0].Instances, nil
}

// TerminateInstances schedules EC2 instances to be terminated.
func (a *API) TerminateInstances(ids []string) error {
	input := &ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice(ids),
	}

	if _, err := a.ec2.TerminateInstances(input); err != nil {
		return err
	}

	return nil
}

func (a *API) CreateTags(resources []string, tags map[string]string) error {
	tagObjs := make([]*ec2.Tag, 0, len(tags))
	for key, value := range tags {
		tagObjs = append(tagObjs, &ec2.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}
	_, err := a.ec2.CreateTags(&ec2.CreateTagsInput{
		Resources: aws.StringSlice(resources),
		Tags:      tagObjs,
	})
	if err != nil {
		return fmt.Errorf("error creating tags: %v", err)
	}
	return err
}
