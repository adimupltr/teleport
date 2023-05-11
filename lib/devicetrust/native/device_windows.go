// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package native

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"github.com/google/go-attestation/attest"
	devicepb "github.com/gravitational/teleport/api/gen/proto/go/teleport/devicetrust/v1"
	"github.com/gravitational/teleport/lib/devicetrust"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math/big"
	"os"
	"os/exec"
	"os/user"
	"path"
	"strings"
)

const (
	deviceStateFolderName  = ".teleport-device"
	attestationKeyFileName = "attestation.key"
)

// Ensures that device state directory exists with the correct permissions and:
// - If it does not exist, creates it.
// - If the directory exists, it has 700 perms or errors.
// - If an attestation key exists, it has 600 perms or errors.
// It returns the absolute path to where the attestation key can be found:
// ~/teleport-device/attestation.key
func setupDeviceStateDir(getHomeDir func() (string, error)) (string, error) {
	home, err := getHomeDir()
	if err != nil {
		return "", trace.Wrap(err)
	}

	deviceStateDirPath := path.Join(home, deviceStateFolderName)
	keyPath := path.Join(deviceStateDirPath, attestationKeyFileName)

	_, err = os.Stat(deviceStateDirPath)
	if err != nil {
		if os.IsNotExist(err) {
			// If it doesn't exist, we can create it and return as we know
			// the perms are correct as we created it.
			if err := os.Mkdir(deviceStateDirPath, 700); err != nil {
				return "", trace.Wrap(err)
			}
			return keyPath, nil
		}
		return "", trace.Wrap(err)
	}

	return keyPath, nil
}

func openTPM() (*attest.TPM, error) {
	cfg := &attest.OpenConfig{
		TPMVersion: attest.TPMVersion20,
		// TODO: Determine if windows command channel wrapper is necessary
	}

	tpm, err := attest.OpenTPM(cfg)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return tpm, nil
}

// getMarshaledEK returns the EK public key in PKIX, ASN.1 DER format.
func getMarshaledEK(tpm *attest.TPM) ([]byte, error) {
	eks, err := tpm.EKs()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if len(eks) == 0 {
		return nil, trace.BadParameter("no endorsement keys found in tpm")
	}
	// TODO(noah): Marshal EK Certificate instead of key if present.
	encodedEK, err := x509.MarshalPKIXPublicKey(eks[0].Public)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return encodedEK, nil
}

// loadOrCreateAK attempts to load an AK from disk. A NotFound error will be
// returned if no such file exists.
func loadAK(
	tpm *attest.TPM,
	persistencePath string,
) (*attest.AK, error) {
	ref, err := os.ReadFile(persistencePath)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}

	ak, err := tpm.LoadAK(ref)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return ak, nil
}

func createAndSaveAK(
	tpm *attest.TPM,
	persistencePath string,
) (*attest.AK, error) {
	ak, err := tpm.NewAK(nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Write it to the well-known location on disk
	ref, err := ak.Marshal()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	err = os.WriteFile(persistencePath, ref, 600)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return ak, nil
}

func enrollDeviceInit() (*devicepb.EnrollDeviceInit, error) {
	akPath, err := setupDeviceStateDir(os.UserHomeDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	tpm, err := openTPM()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer tpm.Close()

	// Try to load an existing AK in the case of re-enrollment, but, if the
	// AK does not exist, create one and persist it.
	ak, err := loadAK(tpm, akPath)
	if err != nil {
		if !trace.IsNotFound(err) {
			return nil, trace.Wrap(err)
		}
		ak, err = createAndSaveAK(tpm, akPath)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	defer ak.Close(tpm)

	deviceData, err := collectDeviceData()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	marshaledEK, err := getMarshaledEK(tpm)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	credentialID, err := credentialIDFromAK(ak)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &devicepb.EnrollDeviceInit{
		CredentialId: credentialID,
		DeviceData:   deviceData,
		Tpm: &devicepb.TPMEnrollPayload{
			Ek: &devicepb.TPMEnrollPayload_EkKey{
				EkKey: marshaledEK,
			},
			AttestationParameters: devicetrust.AttestationParametersToProto(
				ak.AttestationParameters(),
			),
		},
	}, nil
}

// credentialIDFromAK produces a deterministic short-ish unique-ish human
// readable-ish string identifier for a given AK. This can then be used as a
// reference for this AK in the backend.
//
// To produce this, we perform a SHA256 hash over the constituent fields of
// the AKs public key and then base64 encode it to produce a human-readable
// string. This is similar to how SSH fingerprinting of public keys work.
func credentialIDFromAK(ak *attest.AK) (string, error) {
	akPub, err := attest.ParseAKPublic(
		attest.TPMVersion20,
		ak.AttestationParameters().Public,
	)
	if err != nil {
		return "", trace.Wrap(err)
	}
	publicKey := akPub.Public
	switch publicKey := publicKey.(type) {
	case *rsa.PublicKey:
		h := sha256.New()
		// This logic is roughly based off the openssh key fingerprinting,
		// but, the hash excludes "ssh-rsa" and the outputted id is not
		// prepended with "SHA256:
		//
		// It is imperative the order of the fields does not change in future
		// implementations.
		h.Write(big.NewInt(int64(publicKey.E)).Bytes())
		h.Write(publicKey.N.Bytes())
		return base64.RawStdEncoding.EncodeToString(h.Sum(nil)), nil
	default:
		return "", trace.BadParameter("unsupported public key type: %T", publicKey)
	}
}

// getDeviceSerial returns the serial number of the device using PowerShell to
// grab the correct WMI objects. Getting it without calling into PS is possible,
// but requires interfacing with the ancient Win32 COM APIs.
func getDeviceSerial() (string, error) {
	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"Get-WmiObject Win32_BIOS | Select -ExpandProperty SerialNumber",
	)
	// ThinkPad P P14s:
	// PS > Get-WmiObject Win32_BIOS | Select -ExpandProperty SerialNumber
	// PF47WND6
	out, err := cmd.Output()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return strings.TrimSpace(string(out)), nil
}

func getReportedAssetTag() (string, error) {
	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"Get-WmiObject Win32_SystemEnclosure | Select -ExpandProperty SMBIOSAssetTag",
	)
	// ThinkPad P P14s:
	// PS > Get-WmiObject Win32_SystemEnclosure | Select -ExpandProperty SMBIOSAssetTag
	// winaia_1337
	out, err := cmd.Output()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return strings.TrimSpace(string(out)), nil
}

func getDeviceModel() (string, error) {
	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"Get-WmiObject Win32_ComputerSystem | Select -ExpandProperty Model",
	)
	// ThinkPad P P14s:
	// PS> Get-WmiObject Win32_ComputerSystem | Select -ExpandProperty Model
	// 21J50013US
	out, err := cmd.Output()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return strings.TrimSpace(string(out)), nil
}

func getDeviceBaseBoardSerial() (string, error) {
	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"Get-WmiObject Win32_BaseBoard | Select -ExpandProperty SerialNumber",
	)
	// ThinkPad P P14s:
	// PS> Get-WmiObject Win32_BaseBoard | Select -ExpandProperty SerialNumber
	// L1HF2CM03ZT
	out, err := cmd.Output()
	if err != nil {
		return "", trace.Wrap(err)
	}

	return strings.TrimSpace(string(out)), nil
}

func getOSVersion() (string, error) {
	// TODO(noah): Get-ComputerInfo is too slow. Find a faster alternative.
	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"Get-ComputerInfo | Select -ExpandProperty OsVersion",
	)
	// ThinkPad P P14s:
	// PS>  Get-ComputerInfo | Select -ExpandProperty OsVersion
	// 10.0.22621
	out, err := cmd.Output()
	if err != nil {
		return "", trace.Wrap(err)
	}

	return strings.TrimSpace(string(out)), nil
}

func firstOf(strings ...string) string {
	for _, str := range strings {
		if str != "" {
			return str
		}
	}
	return ""
}

func collectDeviceData() (*devicepb.DeviceCollectedData, error) {
	log.Debug("Collecting device data.")
	systemSerial, err := getDeviceSerial()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	model, err := getDeviceModel()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	baseBoardSerial, err := getDeviceBaseBoardSerial()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	reportedAssetTag, err := getReportedAssetTag()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	osVersion, err := getOSVersion()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	u, err := user.Current()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	serial := firstOf(reportedAssetTag, systemSerial, baseBoardSerial)
	if serial == "" {
		return nil, trace.BadParameter("unable to determine serial number")
	}

	dcd := &devicepb.DeviceCollectedData{
		CollectTime:           timestamppb.Now(),
		OsType:                devicepb.OSType_OS_TYPE_WINDOWS,
		SerialNumber:          serial,
		ModelIdentifier:       model,
		OsUsername:            u.Username,
		OsVersion:             osVersion,
		SystemSerialNumber:    systemSerial,
		BaseBoardSerialNumber: baseBoardSerial,
		ReportedAssetTag:      reportedAssetTag,
	}
	log.WithField("device_collected_data", dcd).Debug("Device data collected.")
	return dcd, nil
}

// getDeviceCredential will only return the credential ID on windows. The
// other information is determined server-side.
func getDeviceCredential() (*devicepb.DeviceCredential, error) {
	akPath, err := setupDeviceStateDir(os.UserHomeDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tpm, err := openTPM()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer tpm.Close()

	// Attempt to load the AK from well-known location.
	ak, err := loadAK(tpm, akPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer ak.Close(tpm)

	credentialID, err := credentialIDFromAK(ak)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &devicepb.DeviceCredential{
		Id: credentialID,
	}, nil
}

func solveTPMEnrollChallenge(
	challenge *devicepb.TPMEnrollChallenge,
) (*devicepb.TPMEnrollChallengeResponse, error) {
	akPath, err := setupDeviceStateDir(os.UserHomeDir)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	tpm, err := openTPM()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer tpm.Close()

	// Attempt to load the AK from well-known location.
	ak, err := loadAK(tpm, akPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer ak.Close(tpm)

	// Next perform a platform attestation using the AK.
	log.Debug("Performing platform attestation.")
	platformsParams, err := tpm.AttestPlatform(
		ak,
		challenge.AttestationNonce,
		&attest.PlatformAttestConfig{
			// EventLog == nil indicates that the `attest` package is
			// responsible for providing the eventlog.
			EventLog: nil,
		},
	)
	if err != nil {
		return nil, trace.Wrap(err, "attesting platform")
	}

	// First perform the credential activation challenge provided by the
	// auth server.
	log.Debug("Activating credential.")
	activationSolution, err := ak.ActivateCredential(
		tpm,
		devicetrust.EncryptedCredentialFromProto(challenge.EncryptedCredential),
	)
	if err != nil {
		return nil, trace.Wrap(err, "activating credential with challenge")
	}

	log.Debug("TPM enrollment challenge completed.")
	return &devicepb.TPMEnrollChallengeResponse{
		Solution: activationSolution,
		PlatformParameters: devicetrust.PlatformParametersToProto(
			platformsParams,
		),
	}, nil
}

// signChallenge is not implemented on windows as TPM platform attestation
// is used instead.
func signChallenge(_ []byte) (sig []byte, err error) {
	return nil, devicetrust.ErrPlatformNotSupported
}
