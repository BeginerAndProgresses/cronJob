package main

import (
	"fmt"
	"github.com/IBM/sarama"
	"log"
)

func main() {

}

func sendMessageToKafka(producer sarama.SyncProducer, topic string, message string) {
	// 创建 Kafka 消息
	msg := &sarama.ProducerMessage{
		Topic: topic,
		Value: sarama.StringEncoder(message),
	}

	// 发送消息到 Kafka
	partition, offset, err := producer.SendMessage(msg)
	if err != nil {
		log.Fatalf("Failed to send message to Kafka: %v", err)
	}

	fmt.Printf("Message sent to partition %d with offset %d\n", partition, offset)
}
