/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This package implements atool that finds all of the latest AMIs for a
// release and ensures that they are public.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type Args struct {
	accountId       string
	teleportVersion string
	regions         []string
}

func parseArgs() Args {
	args := Args{}

	kingpin.Flag("aws-account", "AWS Account ID").
		Required().
		StringVar(&args.accountId)

	kingpin.Flag("teleport-version", "Version of teleport AMIs to make public").
		Required().
		StringVar(&args.teleportVersion)

	var regions string
	kingpin.Flag("regions", "A comma-separated list of AWS regions to update").
		Required().
		StringVar(&regions)

	kingpin.Parse()

	args.regions = strings.Split(regions, ",")

	return args
}

func main() {
	args := parseArgs()

	ctx := context.Background()

	for _, region := range args.regions {
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion(region))
		if err != nil {
			log.Fatalf("could not load AWS config for %s: %v", region, err)
		}

		client := ec2.NewFromConfig(cfg)

		for _, edition := range []string{"oss", "ent"} {
			for _, fips := range []string{"false", "true"} {
				// No such combination exists
				if edition == "oss" && fips == "true" {
					continue
				}

				ami, err := findLatestAMI(ctx, client, args.accountId, args.teleportVersion, edition, fips)
				switch {
				case err == nil:
					break

				case errors.Is(err, notFound):
					continue

				default:
					log.Fatalf("Failed to find the latest AMI: %s", err)
				}

				// Mark the AMI as public
				log.Printf("Marking %s as public", ami)
				_, err = client.ModifyImageAttribute(ctx, &ec2.ModifyImageAttributeInput{
					ImageId:   aws.String(ami),
					Attribute: aws.String("launchPermission"),
					LaunchPermission: &types.LaunchPermissionModifications{
						Add: []types.LaunchPermission{
							{Group: types.PermissionGroupAll},
						},
					},
				})
				if err != nil {
					log.Printf("WARNING: Failed to make ami %q public: %s", ami, err)
					continue
				}
			}
		}
	}
}

var notFound error = fmt.Errorf("Not Found")

func findLatestAMI(ctx context.Context, client *ec2.Client, accountId, teleportVersion, edition, fips string) (string, error) {
	resp, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{"teleport-*"}},
			{Name: aws.String("tag:TeleportVersion"), Values: []string{teleportVersion}},
			{Name: aws.String("tag:TeleportEdition"), Values: []string{edition}},
			{Name: aws.String("tag:TeleportFipsEnabled"), Values: []string{fips}},
			{Name: aws.String("tag:BuildType"), Values: []string{"production"}},
		},
		Owners: []string{accountId},
	})

	if err != nil {
		return "", fmt.Errorf("Failed to query AMIs: %w", err)
	}

	if len(resp.Images) == 0 {
		return "", notFound
	}

	// I'm assuming that we will have few enough images returned in
	// any given search thatits not worth setting up a fancy sorting
	// predicate
	newestTimestamp := time.Time{}
	newestAMI := -1
	for i := range resp.Images {
		creationDate, err := time.Parse(time.RFC3339, *resp.Images[i].CreationDate)
		if err != nil {
			return "", fmt.Errorf("failed to parse timestamp %w", err)
		}

		if creationDate.After(newestTimestamp) {
			newestTimestamp = creationDate
			newestAMI = i
		}
	}

	return *resp.Images[newestAMI].ImageId, nil
}
