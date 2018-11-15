// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package method

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

	"github.com/crashlytics/apt-golang-s3/message"
)

const (
	headerCodeCapabilities   = 100
	headerCodeStatus         = 102
	headerCodeURIStart       = 200
	headerCodeURIDone        = 201
	headerCodeURIFailure     = 400
	headerCodeGeneralFailure = 401
	headerCodeURIAcquire     = 600
	headerCodeConfiguration  = 601
)

const (
	headerDescriptionCapabilities   = "Capabilities"
	headerDescriptionStatus         = "Status"
	headerDescriptionURIStart       = "URI Start"
	headerDescriptionURIDone        = "URI Done"
	headerDescriptionURIFailure     = "URI Failure"
	headerDescriptionGeneralFailure = "General Failure"
	headerDescriptionURIAcquire     = "URI Acquire"
	headerDescriptionConfiguration  = "Configuration"
)

const (
	fieldNameCapabilities   = "Capabilities"
	fieldNameConfigItem     = "Config-Item"
	fieldNameSendConfig     = "Send-Config"
	fieldNamePipeline       = "Pipeline"
	fieldNameSingleInstance = "Single-Instance"
	fieldNameURI            = "URI"
	fieldNameFilename       = "Filename"
	fieldNameSize           = "Size"
	fieldNameLastModified   = "Last-Modified"
	fieldNameMessage        = "Message"
	fieldNameMD5Hash        = "MD5-Hash"
	fieldNameMD5SumHash     = "MD5Sum-Hash"
	fieldNameSHA1Hash       = "SHA1-Hash"
	fieldNameSHA256Hash     = "SHA256-Hash"
	fieldNameSHA512Hash     = "SHA512-Hash"
)

const (
	fieldValueTrue       = "true"
	fieldValueYes        = "yes"
	fieldValueNotFound   = "The specified key does not exist."
	fieldValueConnecting = "Connecting to s3.amazonaws.com"
)

type Method struct {
	region  string
	msgChan chan []byte
	wg      *sync.WaitGroup
	stdout  *log.Logger
}

func NewMethod() *Method {
	var wg sync.WaitGroup
	wg.Add(1)
	m := &Method{
		region:  endpoints.UsEast1RegionID,
		msgChan: make(chan []byte),
		wg:      &wg,
		stdout:  log.New(os.Stdout, "", 0),
	}
	return m
}

func (m *Method) Run() {
	m.flushCapabilities()
	go m.readInput(os.Stdin)
	go m.processMessages()
	m.wg.Wait()
}

func (m *Method) flushCapabilities() {
	msg := capabilities()
	m.stdout.Println(msg)
}

func (m *Method) readInput(input io.Reader) {
	scanner := bufio.NewScanner(input)
	buffer := &bytes.Buffer{}
	for {
		hasLine := scanner.Scan()
		if hasLine {
			line := fmt.Sprintf("%s\n", scanner.Text())
			buffer.WriteString(line)
			trimmed := strings.TrimRight(line, "\n")

			// if the trimmed line is empty and the buffer has some content
			// the message is assumed to be complete
			// dispatch it
			if len(trimmed) == 0 && buffer.Len() > 3 {
				m.msgChan <- buffer.Bytes()
				m.wg.Add(1)
				buffer = &bytes.Buffer{}
			}
		} else {
			break
		}
	}
	m.wg.Done()
}

func capabilities() *message.Message {
	header := header(headerCodeCapabilities, headerDescriptionCapabilities)
	fields := []*message.Field{
		field(fieldNameSendConfig, fieldValueTrue),
		field(fieldNamePipeline, fieldValueTrue),
		field(fieldNameSingleInstance, fieldValueYes),
	}
	return &message.Message{Header: header, Fields: fields}
}

func (m *Method) processMessages() {
	for {
		bytes := <-m.msgChan
		go m.handleBytes(bytes)
	}
}

// turn a byte slice into a Message and dispatch
func (m *Method) handleBytes(b []byte) {
	msg, err := message.FromBytes(b)
	m.handleError(err)
	if msg.Header.Status == headerCodeURIAcquire {
		// URI Acquire message
		m.uriAcquire(msg)
	} else if msg.Header.Status == headerCodeConfiguration {
		// Configuration message
		m.configure(msg)
	}
}

//fetch the content from s3 and write it to disk
func (m *Method) uriAcquire(msg *message.Message) {
	uri := msg.GetFieldValue(fieldNameURI)
	s3Uri, err := url.Parse(uri)
	m.handleError(err)
	pathParts := strings.Split(s3Uri.Path, "/")
	bucket := pathParts[1]
	key := strings.Join(pathParts[2:], "/")

	client := m.s3Client(s3Uri)

	headObjectInput := &s3.HeadObjectInput{Bucket: &bucket, Key: &key}
	headObjectOutput, err := client.HeadObject(headObjectInput)
	if err != nil {
		if reqErr, ok := err.(awserr.RequestFailure); ok {
			// A service error occurred
			if reqErr.StatusCode() == 404 {
				m.outputNotFound(s3Uri)
				return
			}
		} else {
			m.handleError(err)
		}
	}

	expectedLength := *headObjectOutput.ContentLength
	lastModified := *headObjectOutput.LastModified
	m.outputURIStart(s3Uri, expectedLength, lastModified)

	filename := msg.GetFieldValue(fieldNameFilename)
	file, err := os.Create(filename)
	m.handleError(err)
	defer file.Close()

	downloader := s3manager.NewDownloaderWithClient(client)
	numBytes, err := downloader.Download(file,
		&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
	m.handleError(err)

	m.outputURIDone(s3Uri, numBytes, lastModified, filename)
}

func (m *Method) s3Client(s3Uri *url.URL) s3iface.S3API {
	awsAccessKeyID := s3Uri.User.Username()
	awsSecretAccessKey, _ := s3Uri.User.Password()
	creds := credentials.NewStaticCredentials(awsAccessKeyID, awsSecretAccessKey, "")
	sess := session.Must(session.NewSession())

	return s3.New(sess, &aws.Config{
		Region:      aws.String(m.region),
		Credentials: creds,
	})
}

// look through the fields and see if there are any that we are interested in
func (m *Method) configure(msg *message.Message) {
	items := msg.GetFieldList(fieldNameConfigItem)
	for _, field := range items {
		aptConfig := strings.Split(field.Value, "=")
		if aptConfig[0] == "Acquire::s3::region" {
			m.region = aptConfig[1]
		}
	}
	m.wg.Done()
}

//102 Status
//URI: s3://fake-access-key-id:fake-secret-access-key@s3.amazonaws.com/bucket-name/apt/trusty/riemann-sumd_0.7.2-1_all.deb
//Message: Connecting to s3.amazonaws.com
func requestStatus(s3Uri *url.URL) *message.Message {
	h := header(headerCodeStatus, headerDescriptionStatus)
	uriField := field(fieldNameURI, s3Uri.String())
	messageField := field(fieldNameMessage, fieldValueConnecting)
	return &message.Message{Header: h, Fields: []*message.Field{uriField, messageField}}
}

//200 URI Start
//URI: s3://fake-access-key-id:fake-secret-access-key@s3.amazonaws.com/bucket-name/apt/trusty/riemann-sumd_0.7.2-1_all.deb
//Size: 9012
//Last-Modified: Thu, 25 Oct 2018 20:17:39 GMT
func (m *Method) uriStart(s3Uri *url.URL, size int64, t time.Time) *message.Message {
	h := header(headerCodeURIStart, headerDescriptionURIStart)
	uriField := field(fieldNameURI, s3Uri.String())
	sizeField := field(fieldNameSize, strconv.FormatInt(size, 10))
	lmField := m.lastModified(t)
	return &message.Message{Header: h, Fields: []*message.Field{uriField, sizeField, lmField}}
}

//201 URI Done
//URI: s3://fake-access-key-id:fake-secret-access-key@s3.amazonaws.com/bucket-name/apt/trusty/riemann-sumd_0.7.2-1_all.deb
//Filename: /var/cache/apt/archives/partial/riemann-sumd_0.7.2-1_all.deb
//Size: 9012
//Last-Modified: Thu, 25 Oct 2018 20:17:39 GMT
//MD5-Hash: 1964cb59e339e7a41cf64e9d40f219b1
//MD5Sum-Hash: 1964cb59e339e7a41cf64e9d40f219b1
//SHA1-Hash: 0d02ab49503be20d153cea63a472c43ebfad2efc
//SHA256-Hash: 92a3f70eb1cf2c69880988a8e74dc6fea7e4f15ee261f74b9be55c866f69c64b
//SHA512-Hash: ab3b1c94618cb58e2147db1c1d4bd3472f17fb11b1361e77216b461ab7d5f5952a5c6bb0443a1507d8ca5ef1eb18ac7552d0f2a537a0d44b8612d7218bf379fb
func (m *Method) uriDone(s3Uri *url.URL, size int64, t time.Time, filename string) *message.Message {
	h := header(headerCodeURIDone, headerDescriptionURIDone)
	uriField := field(fieldNameURI, s3Uri.String())
	filenameField := field(fieldNameFilename, filename)
	sizeField := field(fieldNameSize, strconv.FormatInt(size, 10))
	lmField := m.lastModified(t)
	fileBytes, err := ioutil.ReadFile(filename)
	m.handleError(err)

	fields := []*message.Field{
		uriField,
		filenameField,
		sizeField,
		lmField,
		m.md5Field(fileBytes),
		m.md5SumField(fileBytes),
		m.sha1Field(fileBytes),
		m.sha256Field(fileBytes),
		m.sha512Field(fileBytes),
	}
	return &message.Message{Header: h, Fields: fields}
}

//400 URI Failure
//Message: The specified key does not exist.
//URI: s3://fake-access-key-id:fake-secret-access-key@s3.amazonaws.com/bucket-name/apt/trusty/riemann-sumd_0.7.2-1_all.deb
func notFound(s3Uri *url.URL) *message.Message {
	h := header(headerCodeURIFailure, headerDescriptionURIFailure)
	uriField := field(fieldNameURI, s3Uri.String())
	messageField := field(fieldNameMessage, fieldValueNotFound)
	return &message.Message{Header: h, Fields: []*message.Field{uriField, messageField}}
}

//401 URI Failure
//Message: Error retrieving ...
func generalFailure(err error) *message.Message {
	h := header(headerCodeGeneralFailure, headerDescriptionGeneralFailure)
	messageField := field(fieldNameMessage, err.Error())
	return &message.Message{Header: h, Fields: []*message.Field{messageField}}
}

func (m *Method) outputRequestStatus(s3Uri *url.URL) {
	msg := requestStatus(s3Uri)
	m.stdout.Println(msg.String())
}

func (m *Method) outputURIStart(s3Uri *url.URL, size int64, lastModified time.Time) {
	msg := m.uriStart(s3Uri, size, lastModified)
	m.stdout.Println(msg.String())
}

func (m *Method) outputURIDone(s3Uri *url.URL, size int64, lastModified time.Time, filename string) {
	msg := m.uriDone(s3Uri, size, lastModified, filename)
	m.stdout.Println(msg.String())
	m.wg.Done()
}

func (m *Method) outputNotFound(s3Uri *url.URL) {
	msg := notFound(s3Uri)
	m.stdout.Println(msg.String())
	m.wg.Done()
}

func (m *Method) outputGeneralFailure(err error) {
	msg := generalFailure(err)
	m.stdout.Println(msg.String())
}

func (m *Method) handleError(err error) {
	if err != nil {
		m.outputGeneralFailure(err)
		os.Exit(1)
	}
}

func header(code int, description string) *message.Header {
	return &message.Header{Status: code, Description: description}
}

func field(name string, value string) *message.Field {
	return &message.Field{Name: name, Value: value}
}

func (m *Method) lastModified(t time.Time) *message.Field {
	gmt, err := time.LoadLocation("GMT")
	m.handleError(err)
	return field(fieldNameLastModified, t.In(gmt).Format(time.RFC1123))
}

func (m *Method) md5Field(bytes []byte) *message.Field {
	md5 := md5.New()
	md5String := m.computeHash(md5, bytes)
	return field(fieldNameMD5Hash, md5String)
}

func (m *Method) md5SumField(bytes []byte) *message.Field {
	md5 := md5.New()
	md5String := m.computeHash(md5, bytes)
	return field(fieldNameMD5SumHash, md5String)
}

func (m *Method) sha1Field(bytes []byte) *message.Field {
	sha1 := sha1.New()
	sha1String := m.computeHash(sha1, bytes)
	return field(fieldNameSHA1Hash, sha1String)
}

func (m *Method) sha256Field(bytes []byte) *message.Field {
	sha256 := sha256.New()
	sha256String := m.computeHash(sha256, bytes)
	return field(fieldNameSHA256Hash, sha256String)
}

func (m *Method) sha512Field(bytes []byte) *message.Field {
	sha512 := sha512.New()
	sha512String := m.computeHash(sha512, bytes)
	return field(fieldNameSHA512Hash, sha512String)
}

func (m *Method) computeHash(h hash.Hash, fileBytes []byte) string {
	m.prepareHash(h, fileBytes)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (m *Method) prepareHash(h hash.Hash, fileBytes []byte) {
	if _, err := io.Copy(h, bytes.NewReader(fileBytes)); err != nil {
		m.handleError(err)
	}
}