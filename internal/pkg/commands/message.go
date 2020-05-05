/*
 * Copyright 2019-present Ciena Corporation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Shopify/sarama"
	"github.com/fullstorydev/grpcurl"
	"github.com/golang/protobuf/ptypes/any"
	flags "github.com/jessevdk/go-flags"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/opencord/voltctl/pkg/filter"
	"github.com/opencord/voltctl/pkg/format"
	"github.com/opencord/voltctl/pkg/model"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"
)

/*
 * The "message listen" command supports two types of output:
 *    1) A summary output where a row is displayed for each message received. For the summary
 *       format, DEFAULT_MESSAGE_FORMAT contains the default list of columns that will be
 *       display and can be overridden at runtime.
 *    2) A body output where the full grpcurl or json body is output for each message received.
 *
 * These two modes are switched by using the "-b" / "--body" flag.
 *
 * The summary mode has the potential to aggregate data together from multiple parts of the
 * message. For example, it currently aggregates the InterAdapterHeader contents together with
 * the InterContainerHeader contents.
 *
 * Similar to "event listen", the  "message listen" command operates in a streaming mode, rather
 * than collecting a list of results and then emitting them at program exit. This is done to
 * facilitate options such as "-F" / "--follow" where the program is intended to  operate
 * continuously. This means that automatically calculating column widths is not practical, and
 * a set of Fixed widths (MessageHeaderDefaultWidths) are predefined.
 *
 * As there are multiple kafka topics that can be listened to, specifying a topic is a
 * mandatory positional argument for the `message listen` command. Common topics include:
 *   * openolt
 *   * brcm_openonu_adapter
 *   * rwcore
 *   * core-pair-1
 */

const (
	DEFAULT_MESSAGE_FORMAT = "table{{.Id}}\t{{.Type}}\t{{.FromTopic}}\t{{.ToTopic}}\t{{.KeyTopic}}\t{{.InterAdapterType}}"
)

type MessageListenOpts struct {
	Format string `long:"format" value-name:"FORMAT" default:"" description:"Format to use to output structured data"`
	// nolint: staticcheck
	OutputAs string `short:"o" long:"outputas" default:"table" choice:"table" choice:"json" choice:"yaml" description:"Type of output to generate"`
	Filter   string `short:"f" long:"filter" default:"" value-name:"FILTER" description:"Only display results that match filter"`
	Follow   bool   `short:"F" long:"follow" description:"Continue to consume until CTRL-C is pressed"`
	ShowBody bool   `short:"b" long:"show-body" description:"Show body of messages rather than only a header summary"`
	Count    int    `short:"c" long:"count" default:"-1" value-name:"LIMIT" description:"Limit the count of messages that will be printed"`
	Now      bool   `short:"n" long:"now" description:"Stop printing messages when current time is reached"`
	Timeout  int    `short:"t" long:"idle" default:"900" value-name:"SECONDS" description:"Timeout if no message received within specified seconds"`
	Since    string `short:"s" long:"since" default:"" value-name:"TIMESTAMP" description:"Do not show entries before timestamp"`

	Args struct {
		Topic string
	} `positional-args:"yes" required:"yes"`
}

type MessageOpts struct {
	MessageListen MessageListenOpts `command:"listen"`
}

var interAdapterOpts = MessageOpts{}

/* MessageHeader is a set of fields extracted
 * from voltha.MessageHeader as well as useful other
 * places such as InterAdapterHeader. These are fields that
 * will be summarized in list mode and/or can be filtered
 * on.
 */
type MessageHeader struct {
	Id               string    `json:"id"`
	Type             string    `json:"type"`
	FromTopic        string    `json:"from_topic"`
	ToTopic          string    `json:"to_topic"`
	KeyTopic         string    `json:"key_topic"`
	Timestamp        time.Time `json:"timestamp"`
	InterAdapterType string    `json:"inter_adapter_type"` // interadapter header
	ToDeviceId       string    `json:"to_device_id"`       // interadapter header
	ProxyDeviceId    string    `json:"proxy_device_id"`    //interadapter header
}

/* Fixed widths because we output in a continuous streaming
 * mode rather than a table-based dump at the end.
 */
type MessageHeaderWidths struct {
	Id               int
	Type             int
	FromTopic        int
	ToTopic          int
	KeyTopic         int
	InterAdapterType int
	ToDeviceId       int
	ProxyDeviceId    int
	Timestamp        int
}

var DefaultMessageWidths MessageHeaderWidths = MessageHeaderWidths{
	Id:               32,
	Type:             10,
	FromTopic:        16,
	ToTopic:          16,
	KeyTopic:         10,
	Timestamp:        10,
	InterAdapterType: 14,
	ToDeviceId:       10,
	ProxyDeviceId:    10,
}

func RegisterMessageCommands(parent *flags.Parser) {
	if _, err := parent.AddCommand("message", "message commands", "Commands for observing messages between components", &interAdapterOpts); err != nil {
		Error.Fatalf("Unable to register message commands with voltctl command parser: %s", err.Error())
	}
}

// Find the any.Any field named by "fieldName" in the dynamic Message m.
// Create a new dynamic message using the bytes from the Any
// Return the new dynamic message and the type name
func DeserializeAny(icFile *desc.FileDescriptor, m *dynamic.Message, fieldName string) (*dynamic.Message, string, error) {
	f, err := m.TryGetFieldByName(fieldName)
	if err != nil {
		return nil, "", err
	}
	a := f.(*any.Any)
	embeddedType := strings.SplitN(a.TypeUrl, "/", 2)[1] // example type.googleapis.com/voltha.InterContainerRequestBody
	embeddedBytes := a.Value

	md := icFile.FindMessage(embeddedType)

	embeddedM := dynamic.NewMessage(md)
	err = embeddedM.Unmarshal(embeddedBytes)
	if err != nil {
		return nil, "", err
	}

	return embeddedM, embeddedType, nil
}

// Extract the header, as well as a few other items that might be of interest
func DecodeInterContainerHeader(icFile *desc.FileDescriptor, md *desc.MessageDescriptor, b []byte, ts time.Time, f grpcurl.Formatter) (*MessageHeader, error) {
	m := dynamic.NewMessage(md)
	if err := m.Unmarshal(b); err != nil {
		return nil, err
	}

	headerIntf, err := m.TryGetFieldByName("header")
	if err != nil {
		return nil, err
	}

	header := headerIntf.(*dynamic.Message)

	idIntf, err := header.TryGetFieldByName("id")
	if err != nil {
		return nil, err
	}
	id := idIntf.(string)

	typeIntf, err := header.TryGetFieldByName("type")
	if err != nil {
		return nil, err
	}
	msgType := typeIntf.(int32)

	fromTopicIntf, err := header.TryGetFieldByName("from_topic")
	if err != nil {
		return nil, err
	}
	fromTopic := fromTopicIntf.(string)

	toTopicIntf, err := header.TryGetFieldByName("to_topic")
	if err != nil {
		return nil, err
	}
	toTopic := toTopicIntf.(string)

	keyTopicIntf, err := header.TryGetFieldByName("key_topic")
	if err != nil {
		return nil, err
	}
	keyTopic := keyTopicIntf.(string)

	timestampIntf, err := header.TryGetFieldByName("timestamp")
	if err != nil {
		return nil, err
	}
	timestamp, err := DecodeTimestamp(timestampIntf)
	if err != nil {
		return nil, err
	}

	// Pull some additional data out of the InterAdapterHeader, if it
	// is embedded inside the InterContainerMessage

	var iaMessageTypeStr string
	var toDeviceId string
	var proxyDeviceId string
	body, bodyKind, err := DeserializeAny(icFile, m, "body")
	if err != nil {
		return nil, err
	}
	switch bodyKind {
	case "voltha.InterContainerRequestBody":
		argListIntf, err := body.TryGetFieldByName("args")
		if err != nil {
			return nil, err
		}
		argList := argListIntf.([]interface{})
		for _, argIntf := range argList {
			arg := argIntf.(*dynamic.Message)
			keyIntf, err := arg.TryGetFieldByName("key")
			if err != nil {
				return nil, err
			}
			key := keyIntf.(string)
			if key == "msg" {
				argBody, argBodyKind, err := DeserializeAny(icFile, arg, "value")
				if err != nil {
					return nil, err
				}
				switch argBodyKind {
				case "voltha.InterAdapterMessage":
					iaHeaderIntf, err := argBody.TryGetFieldByName("header")
					if err != nil {
						return nil, err
					}
					iaHeader := iaHeaderIntf.(*dynamic.Message)
					iaMessageTypeIntf, err := iaHeader.TryGetFieldByName("type")
					if err != nil {
						return nil, err
					}
					iaMessageType := iaMessageTypeIntf.(int32)
					iaMessageTypeStr, err = model.GetEnumString(iaHeader, "type", iaMessageType)
					if err != nil {
						return nil, err
					}

					toDeviceIdIntf, err := iaHeader.TryGetFieldByName("to_device_id")
					if err != nil {
						return nil, err
					}
					toDeviceId = toDeviceIdIntf.(string)

					proxyDeviceIdIntf, err := iaHeader.TryGetFieldByName("proxy_device_id")
					if err != nil {
						return nil, err
					}
					proxyDeviceId = proxyDeviceIdIntf.(string)
				}
			}
		}
	}

	messageHeaderType, err := model.GetEnumString(header, "type", msgType)
	if err != nil {
		return nil, err
	}

	icHeader := MessageHeader{Id: id,
		Type:             messageHeaderType,
		FromTopic:        fromTopic,
		ToTopic:          toTopic,
		KeyTopic:         keyTopic,
		Timestamp:        timestamp,
		InterAdapterType: iaMessageTypeStr,
		ProxyDeviceId:    proxyDeviceId,
		ToDeviceId:       toDeviceId,
	}

	return &icHeader, nil
}

// Print the full message, either in JSON or in GRPCURL-human-readable format,
// depending on which grpcurl formatter is passed in.
func PrintInterContainerMessage(f grpcurl.Formatter, md *desc.MessageDescriptor, b []byte) error {
	m := dynamic.NewMessage(md)
	err := m.Unmarshal(b)
	if err != nil {
		return err
	}
	s, err := f(m)
	if err != nil {
		return err
	}
	fmt.Println(s)
	return nil
}

// Print just the enriched InterContainerHeader. This is either in JSON format, or in
// table format.
func PrintInterContainerHeader(outputAs string, outputFormat string, hdr *MessageHeader) error {
	if outputAs == "json" {
		asJson, err := json.Marshal(hdr)
		if err != nil {
			return fmt.Errorf("Error marshalling JSON: %v", err)
		} else {
			fmt.Printf("%s\n", asJson)
		}
	} else {
		f := format.Format(outputFormat)
		output, err := f.ExecuteFixedWidth(DefaultMessageWidths, false, *hdr)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", output)
	}
	return nil
}

// Get the FileDescriptor that has the InterContainer protos
func GetInterContainerDescriptorFile() (*desc.FileDescriptor, error) {
	descriptor, err := GetDescriptorSource()
	if err != nil {
		return nil, err
	}

	// get the symbol for voltha.InterContainerMessage
	iaSymbol, err := descriptor.FindSymbol("voltha.InterContainerMessage")
	if err != nil {
		return nil, err
	}

	icFile := iaSymbol.GetFile()
	return icFile, nil
}

// Start output, print any column headers or other start characters
func (options *MessageListenOpts) StartOutput(outputFormat string) error {
	if options.OutputAs == "json" {
		fmt.Println("[")
	} else if (options.OutputAs == "table") && !options.ShowBody {
		f := format.Format(outputFormat)
		output, err := f.ExecuteFixedWidth(DefaultMessageWidths, true, nil)
		if err != nil {
			return err
		}
		fmt.Println(output)
	}
	return nil
}

// Finish output, print any column footers or other end characters
func (options *MessageListenOpts) FinishOutput() {
	if options.OutputAs == "json" {
		fmt.Println("]")
	}
}

func (options *MessageListenOpts) Execute(args []string) error {
	ProcessGlobalOptions()
	if GlobalConfig.Kafka == "" {
		return errors.New("Kafka address is not specified")
	}

	icFile, err := GetInterContainerDescriptorFile()
	if err != nil {
		return err
	}

	icMessage := icFile.FindMessage("voltha.InterContainerMessage")

	config := sarama.NewConfig()
	config.ClientID = "go-kafka-consumer"
	config.Consumer.Return.Errors = true
	config.Version = sarama.V1_0_0_0
	brokers := []string{GlobalConfig.Kafka}

	client, err := sarama.NewClient(brokers, config)
	if err != nil {
		return err
	}

	defer func() {
		if err := client.Close(); err != nil {
			panic(err)
		}
	}()

	consumer, consumerErrors, highwaterMarks, err := startInterContainerConsumer([]string{options.Args.Topic}, client)
	if err != nil {
		return err
	}

	highwater := highwaterMarks[options.Args.Topic]

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	// Count how many message processed
	consumeCount := 0

	// Count how many messages were printed
	count := 0

	var grpcurlFormatter grpcurl.Formatter
	// need a descriptor source, any method will do
	descriptor, _, err := GetMethod("device-list")
	if err != nil {
		return err
	}

	jsonFormatter := grpcurl.NewJSONFormatter(false, grpcurl.AnyResolverFromDescriptorSource(descriptor))
	if options.ShowBody {
		if options.OutputAs == "json" {
			grpcurlFormatter = jsonFormatter
		} else {
			grpcurlFormatter = grpcurl.NewTextFormatter(false)
		}
	}

	var headerFilter *filter.Filter
	if options.Filter != "" {
		headerFilterVal, err := filter.Parse(options.Filter)
		if err != nil {
			return fmt.Errorf("Failed to parse filter: %v", err)
		}
		headerFilter = &headerFilterVal
	}

	outputFormat := CharReplacer.Replace(options.Format)
	if outputFormat == "" {
		outputFormat = GetCommandOptionWithDefault("intercontainer-listen", "format", DEFAULT_MESSAGE_FORMAT)
	}

	err = options.StartOutput(outputFormat)
	if err != nil {
		return err
	}

	var since *time.Time
	if options.Since != "" {
		since, err = ParseSince(options.Since)
		if err != nil {
			return err
		}
	}

	// Get signnal for finish
	doneCh := make(chan struct{})
	go func() {
		tStart := time.Now()
	Loop:
		for {
			// Initialize the idle timeout timer
			timeoutTimer := time.NewTimer(time.Duration(options.Timeout) * time.Second)
			select {
			case msg := <-consumer:
				consumeCount++
				hdr, err := DecodeInterContainerHeader(icFile, icMessage, msg.Value, msg.Timestamp, jsonFormatter)
				if err != nil {
					log.Printf("Error decoding header %v\n", err)
					continue
				}
				if headerFilter != nil && !headerFilter.Evaluate(*hdr) {
					// skip printing message
				} else if since != nil && hdr.Timestamp.Before(*since) {
					// it's too old
				} else {
					// comma separated between this message and predecessor
					if count > 0 {
						if options.OutputAs == "json" {
							fmt.Println(",")
						}
					}

					// Print it
					if options.ShowBody {
						if err := PrintInterContainerMessage(grpcurlFormatter, icMessage, msg.Value); err != nil {
							log.Printf("%v\n", err)
						}
					} else {
						if err := PrintInterContainerHeader(options.OutputAs, outputFormat, hdr); err != nil {
							log.Printf("%v\n", err)
						}
					}

					// Check to see if we've hit the "count" threshold the user specified
					count++
					if (options.Count > 0) && (count >= options.Count) {
						log.Println("Count reached")
						doneCh <- struct{}{}
						break Loop
					}

					// Check to see if we've hit the "now" threshold the user specified
					if (options.Now) && (!hdr.Timestamp.Before(tStart)) {
						log.Println("Now timestamp reached")
						doneCh <- struct{}{}
						break Loop
					}
				}

				// If we're not in follow mode, see if we hit the highwater mark
				if !options.Follow && !options.Now && (msg.Offset >= highwater) {
					log.Println("High water reached")
					doneCh <- struct{}{}
					break Loop
				}

				// Reset the timeout timer
				if !timeoutTimer.Stop() {
					<-timeoutTimer.C
				}
			case consumerError := <-consumerErrors:
				log.Printf("Received consumerError topic=%v, partition=%v, err=%v\n", string(consumerError.Topic), string(consumerError.Partition), consumerError.Err)
				doneCh <- struct{}{}
			case <-signals:
				doneCh <- struct{}{}
			case <-timeoutTimer.C:
				log.Printf("Idle timeout\n")
				doneCh <- struct{}{}
			}
		}
	}()

	<-doneCh

	options.FinishOutput()

	log.Printf("Consumed %d messages. Printed %d messages", consumeCount, count)

	return nil
}

// Consume message from Sarama and send them out on a channel.
// Supports multiple topics.
// Taken from Sarama example consumer.
func startInterContainerConsumer(topics []string, client sarama.Client) (chan *sarama.ConsumerMessage, chan *sarama.ConsumerError, map[string]int64, error) {
	master, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		return nil, nil, nil, err
	}

	consumers := make(chan *sarama.ConsumerMessage)
	errors := make(chan *sarama.ConsumerError)
	highwater := make(map[string]int64)
	for _, topic := range topics {
		if strings.Contains(topic, "__consumer_offsets") {
			continue
		}
		partitions, _ := master.Partitions(topic)

		// TODO: Add support for multiple partitions
		if len(partitions) > 1 {
			log.Printf("WARNING: %d partitions on topic %s but we only listen to the first one\n", len(partitions), topic)
		}

		hw, err := client.GetOffset("openolt", partitions[0], sarama.OffsetNewest)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("Error in consume() getting highwater: Topic %v Partitions: %v", topic, partitions)
		}
		highwater[topic] = hw

		consumer, err := master.ConsumePartition(topic, partitions[0], sarama.OffsetOldest)
		if nil != err {
			return nil, nil, nil, fmt.Errorf("Error in consume(): Topic %v Partitions: %v", topic, partitions)
		}
		log.Println(" Start consuming topic ", topic)
		go func(topic string, consumer sarama.PartitionConsumer) {
			for {
				select {
				case consumerError := <-consumer.Errors():
					errors <- consumerError

				case msg := <-consumer.Messages():
					consumers <- msg
				}
			}
		}(topic, consumer)
	}

	return consumers, errors, highwater, nil
}
