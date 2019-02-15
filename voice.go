package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/oleiade/lane"
	"github.com/pkg/errors"
	"github.com/rylio/ytdl"
)

const (
	channels  int = 2
	frameRate int = 48000
	frameSize int = 960
)

type VoiceClient struct {
	discord    *discord
	voice      *discordgo.VoiceConnection
	history    *lane.Queue
	queue      *lane.Queue
	pcmChannel chan []int16
	serverId   string
	skip       bool
	stop       bool
	isPlaying  bool
}

func newVoiceClient(d *discord) *VoiceClient {
	return &VoiceClient{
		discord:    d,
		history:    lane.NewQueue(),
		queue:      lane.NewQueue(),
		pcmChannel: make(chan []int16, 2),
	}
}

func (vc *VoiceClient) connectVoice(guildId, channelId string) error {
	voice, err := vc.discord.ChannelVoiceJoin(guildId, channelId, false, false)
	if err != nil {
		return err
	}

	vc.voice = voice

	go SendPCM(vc.voice, vc.pcmChannel)

	return nil
}

func (vc *VoiceClient) Disconnect() {
	if vc.isPlaying {
		vc.StopVideo()

		// wait a little bit~
		time.Sleep(250 * time.Millisecond)
	}

	close(vc.pcmChannel)

	if vc.voice != nil {
		vc.voice.Disconnect()
	}
}

func (vc *VoiceClient) ResumeVideo() {
	vc.stop = false

	link := vc.history.Dequeue()
	if link != nil {
		vc.playVideo(link.(string))
	}
}

func (vc *VoiceClient) StopVideo() {
	vc.stop = true
}

func (vc *VoiceClient) SkipVideo() {
	vc.skip = true
}

func (vc *VoiceClient) queueVideo(audioLink string) {
	vc.queue.Enqueue(audioLink)
	go vc.processQueue()
}

func (vc *VoiceClient) PlayQuery(query string) ([]string, error) {
	// if the query is a youtube playlist link
	// fetch the videos from the youtube api
	if playlistId, err := getYouTubePlayListIdFromURL(query); err == nil {
		videos, err := playlistVideos(playlistId)
		if err != nil {
			return []string{}, err
		}

		return vc.playYoutubeList(videos)
	}

	if !isYouTubeLink(query) {
		// check if an API Key was configured
		// if it isn't searching can't be done, so quit early
		if config.YouTubeKey == "" {
			return []string{}, errors.New("youtube searching has not been configured, needs API key")
		}

		// if these are just words to search for
		// search with the youtube api
		resp, err := searchByKeywords(query)
		if err != nil {
			return []string{}, err
		}

		query = resp.VideoId
	}

	// if its just a regular youtube link
	// pass it along
	title, err := vc.playYoutubeWithId(query)
	return []string{title}, err
}

func (vc *VoiceClient) playYoutubeWithId(videoId string) (string, error) {
	info, err := ytdl.GetVideoInfo(videoId)
	if err != nil {
		return "", err
	}

	fmt.Printf("Queuing Video: %s [%s]\n", info.Title, videoId)

	audioLink, err := getSortYouTubeAudioLink(info)
	if err != nil {
		return "", err
	}

	vc.queueVideo(audioLink.String())

	return info.Title, nil
}

func (vc *VoiceClient) playYoutubeList(videos []string) ([]string, error) {
	var titleVideos []string

	for _, video := range videos {
		title, err := vc.playYoutubeWithId(video)
		if err != nil {
			log.Println(err)
			continue
		}

		titleVideos = append(titleVideos, title)
	}

	return titleVideos, nil
}

func (vc *VoiceClient) playVideo(url string) {
	vc.isPlaying = true

	// fetch music stream with http
	resp, err := http.Get(url)
	if err != nil {
		log.Println(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("http status %d (non-200)", resp.StatusCode)
	}

	// stream input to ffmpeg
	run := exec.Command("ffmpeg", "-i", "-", "-f", "s16le", "-ar", strconv.Itoa(frameRate), "-ac", strconv.Itoa(channels), "pipe:1")
	run.Stdin = resp.Body

	stdout, err := run.StdoutPipe()
	if err != nil {
		fmt.Printf("ffmpeg failed to pipe out: %s\n", err.Error())
		return
	}

	err = run.Start()
	if err != nil {
		fmt.Printf("ffmpeg failed to start: %s\n", err.Error())
		return
	}

	defer run.Process.Kill()

	audiobuf := make([]int16, frameSize*channels)

	vc.voice.Speaking(true)
	defer vc.voice.Speaking(false)

	for {
		// read data from ffmpeg
		err = binary.Read(stdout, binary.LittleEndian, &audiobuf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			log.Println("oops, failed playing", err)
			break
		}

		if err != nil {
			log.Println("oops, failed playing", err)
			break
		}

		if vc.stop == true || vc.skip == true {
			log.Println("stopped playing")
			break
		}

		vc.pcmChannel <- audiobuf
	}

	vc.isPlaying = false
}

func (vc *VoiceClient) processQueue() {
	if vc.isPlaying {
		return
	}

	for {
		vc.skip = false
		if link := vc.queue.Dequeue(); link != nil && !vc.stop {
			vc.history.Enqueue(link.(string))
			vc.playVideo(link.(string))
		} else {
			break
		}
	}
}
