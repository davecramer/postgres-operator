package util

/*
 Copyright 2019 - 2020 Crunchy Data Solutions, Inc.
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

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"

	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	"github.com/crunchydata/postgres-operator/config"
	"github.com/crunchydata/postgres-operator/kubeapi"
	"github.com/crunchydata/postgres-operator/sshutil"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// BackrestRepoConfig represents the configuration required to created backrest repo secrets
type BackrestRepoConfig struct {
	BackrestS3Key       string
	BackrestS3KeySecret string
	ClusterName         string
	ClusterNamespace    string
	OperatorNamespace   string
}

// AWSS3Secret is a representation of the yaml structure found in aws-s3-credentials.yaml
// for providing an AWS S3 key and key secret
type AWSS3Secret struct {
	AWSS3Key       string `yaml:"aws-s3-key"`
	AWSS3KeySecret string `yaml:"aws-s3-key-secret"`
}

const (
	// DefaultGeneratedPasswordLength is the length of what a generated password
	// is if it's not set in the pgo.yaml file, and to create some semblance of
	// consistency
	DefaultGeneratedPasswordLength = 24
	// DefaultPasswordValidUntilDays is the number of days until a PostgreSQL user's
	// password expires. If it is not set in the pgo.yaml file, we will use a
	// default of "0" which means that a password will never expire
	DefaultPasswordValidUntilDays = 0
)

const (
	// SQLValidUntilAlways uses a special PostgreSQL value to ensure a password
	// is always valid
	SQLValidUntilAlways = "infinity"
	// SQLValidUntilNever uses a special PostgreSQL value to ensure a password
	// is never valid. This is exportable and used in other places
	SQLValidUntilNever = "-infinity"
	// sqlSetPasswordDefault is the SQL to update the password
	// NOTE: this is safe from SQL injection as we explicitly add the inerpolated
	// string as a MD5 hash or SCRAM verifier. And if you're not doing that,
	// rethink your usage of this
	//
	// The escaping for SQL injections is handled in the SetPostgreSQLPassword
	// function
	sqlSetPasswordDefault = `ALTER ROLE %s PASSWORD %s;`
)

// CreateBackrestRepoSecrets creates the secrets required to manage the
// pgBackRest repo container
func CreateBackrestRepoSecrets(clientset *kubernetes.Clientset,
	backrestRepoConfig BackrestRepoConfig) error {

	keys, err := sshutil.NewPrivatePublicKeyPair()
	if err != nil {
		return err
	}

	// Retrieve the S3/SSHD configuration files from secret
	configs, _, err := kubeapi.GetSecret(clientset, "pgo-backrest-repo-config",
		backrestRepoConfig.OperatorNamespace)
	if kerrors.IsNotFound(err) || err != nil {
		return err
	}

	// if an S3 key has been provided via the request, then use key and key secret inlcuded
	// in the request instead of the default credentials provided by 'aws-s3-credentials.yaml'.
	// If an S3 key doesn't exist in the request, the use `aws-s3-credentials.yaml`
	var s3KeySecretData []byte
	if backrestRepoConfig.BackrestS3Key != "" && backrestRepoConfig.BackrestS3KeySecret != "" {
		s3Secret := AWSS3Secret{
			backrestRepoConfig.BackrestS3Key,
			backrestRepoConfig.BackrestS3KeySecret,
		}
		s3KeySecretData, err = yaml.Marshal(s3Secret)
		if err != nil {
			return err
		}
	} else {
		s3KeySecretData = configs.Data["aws-s3-credentials.yaml"]
	}

	secret := v1.Secret{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", backrestRepoConfig.ClusterName,
				config.LABEL_BACKREST_REPO_SECRET),
			Labels: map[string]string{
				config.LABEL_VENDOR:            config.LABEL_CRUNCHY,
				config.LABEL_PG_CLUSTER:        backrestRepoConfig.ClusterName,
				config.LABEL_PGO_BACKREST_REPO: "true",
			},
		},
		Data: map[string][]byte{
			"authorized_keys":         keys.Public,
			"aws-s3-ca.crt":           configs.Data["aws-s3-ca.crt"],
			"aws-s3-credentials.yaml": s3KeySecretData,
			"config":                  configs.Data["config"],
			"id_ed25519":              keys.Private,
			"sshd_config":             configs.Data["sshd_config"],
			"ssh_host_ed25519_key":    keys.Private,
		},
	}
	return kubeapi.CreateSecret(clientset, &secret, backrestRepoConfig.ClusterNamespace)
}

// IsAutofailEnabled - returns true if autofail label is set to true, false if not.
func IsAutofailEnabled(cluster *crv1.Pgcluster) bool {

	labels := cluster.ObjectMeta.Labels
	failLabel := labels[config.LABEL_AUTOFAIL]

	log.Debugf("IsAutoFailEnabled: %s", failLabel)

	return failLabel == "true"
}

// GeneratedPasswordValidUntilDays returns the value for the number of days that
// a password is valid for, which is used as part of PostgreSQL's VALID UNTIL
// directive on a user.  It first determines if the user provided this value via
// a configuration file, and if not and/or the value is invalid, uses the
// default value
func GeneratedPasswordValidUntilDays(configuredValidUntilDays string) int {
	// set the generated password length for random password generation
	// note that "configuredPasswordLength" may be an empty string, and as such
	// the below line could fail. That's ok though! as we have a default set up
	validUntilDays, err := strconv.Atoi(configuredValidUntilDays)

	// if there is an error...set it to a default
	if err != nil {
		validUntilDays = DefaultPasswordValidUntilDays
	}

	return validUntilDays
}

// GetPrimaryPod gets the Pod of the primary PostgreSQL instance. If somehow
// the query gets multiple pods, then the first one in the list is returned
func GetPrimaryPod(clientset *kubernetes.Clientset, cluster *crv1.Pgcluster) (*v1.Pod, error) {
	// set up the selector for the primary pod
	selector := fmt.Sprintf("%s=%s,%s=master",
		config.LABEL_PG_CLUSTER, cluster.Spec.Name, config.LABEL_PGHA_ROLE)
	namespace := cluster.Spec.Namespace

	// query the pods
	pods, err := kubeapi.GetPods(clientset, selector, namespace)

	// if there is an error, log it and abort
	if err != nil {
		return nil, err
	}

	// if no pods are retirn, then also raise an error
	if len(pods.Items) == 0 {
		err := errors.New(fmt.Sprintf("primary pod not found for selector [%s]", selector))
		return nil, err
	}

	// Grab the first pod from the list as this is presumably the primary pod
	pod := pods.Items[0]
	return &pod, nil
}

// GetS3CredsFromBackrestRepoSecret retrieves the AWS S3 credentials, i.e. the key and key
// secret, from a specific cluster's backrest repo secret
func GetS3CredsFromBackrestRepoSecret(clientset *kubernetes.Clientset, clusterName,
	namespace string) (s3Secret AWSS3Secret, err error) {

	currBackrestSecret, _, err := kubeapi.GetSecret(clientset,
		clusterName+"-backrest-repo-config", namespace)
	if err != nil {
		log.Error(err)
		return
	}
	err = yaml.Unmarshal(currBackrestSecret.Data["aws-s3-credentials.yaml"], &s3Secret)
	if err != nil {
		log.Error(err)
		return
	}

	return
}

// SetPostgreSQLPassword updates the password for a PostgreSQL role in the
// PostgreSQL cluster by executing into the primary Pod and changing it
//
// Note: it is recommended to pre-hash the password (e.g. md5, SCRAM) so that
// way the plaintext password is not logged anywhere. This also avoids potential
// SQL injections
func SetPostgreSQLPassword(clientset *kubernetes.Clientset, restconfig *rest.Config, pod *v1.Pod, username, password, sqlCustom string) error {
	log.Debugf("set PostgreSQL password for user [%s]", username)

	// if custom SQL is not set, use the default SQL
	sqlRaw := sqlCustom

	if sqlRaw == "" {
		sqlRaw = sqlSetPasswordDefault
	}

	// This is safe from SQL injection as we are using constants and a well defined
	// string...well, as long as the function caller does this
	sql := strings.NewReader(fmt.Sprintf(sqlRaw,
		SQLQuoteIdentifier(username), SQLQuoteLiteral(password)))
	cmd := []string{"psql"}

	// exec into the pod to run the query
	_, stderr, err := kubeapi.ExecToPodThroughAPI(restconfig, clientset,
		cmd, "database", pod.Name, pod.ObjectMeta.Namespace, sql)

	// if there is an error executing the command, or output in stderr,
	// log the error message and return
	if err != nil {
		log.Error(err)
		return err
	} else if stderr != "" {
		log.Error(stderr)
		return fmt.Errorf(stderr)
	}

	return nil
}
