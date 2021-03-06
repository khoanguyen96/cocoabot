package main

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/khoanguyen96/cocoabot/audio"
	"github.com/oleiade/lane"
	"github.com/pkg/errors"
	"github.com/rylio/ytdl"
)

// VoiceClient is a custom voice instance for a Discord voice channel.
type VoiceClient struct {
	discord   *discord
	voice     *discordgo.VoiceConnection
	queue     *lane.Queue
	mutex     sync.Mutex
	serverId  string
	skip      bool
	stop      bool
	isPlaying bool
}

func newVoiceClient(d *discord) *VoiceClient {
	return &VoiceClient{
		discord: d,
		queue:   lane.NewQueue(),
	}
}

func (vc *VoiceClient) connectVoice(guildId, channelId string) error {
	voice, err := vc.discord.ChannelVoiceJoin(guildId, channelId, false, false)
	if err != nil {
		return err
	}

	vc.voice = voice

	return nil
}

// Disconnect tells the bot to disconnect from the voice channel.
func (vc *VoiceClient) Disconnect() {
	if vc.isPlaying {
		vc.StopVideo()

		// wait a little bit~
		time.Sleep(250 * time.Millisecond)
	}

	if vc.voice != nil {
		vc.voice.Disconnect()
	}
}

// StopVideo tells the bot to stop the current song and clear the queue.
func (vc *VoiceClient) StopVideo() {
	vc.stop = true
}

// SkipVideo tells the bot to skip the current song and go to the next song in the queue.
func (vc *VoiceClient) SkipVideo() {
	vc.skip = true
}

func (vc *VoiceClient) queueVideo(sq SongRequest) {
	vc.queue.Enqueue(sq)
	go vc.processQueue()
}

// PlayQuery takes a SongRequest and attempts to search and play it.
func (vc *VoiceClient) PlayQuery(query SongRequest) ([]string, error) {
	// if the query is a youtube playlist link
	// fetch the videos from the youtube api
	if playlistId, err := getYouTubePlayListIdFromURL(query.SongQuery); err == nil {
		videos, err := playlistVideos(playlistId)
		if err != nil {
			return []string{}, err
		}

		return vc.playYoutubeList(videos, query)
	}

	if !isYouTubeLink(query.SongQuery) {
		// check if an API Key was configured
		// if it isn't searching can't be done, so quit early
		if config.YouTubeKey == "" {
			return []string{}, errors.New("youtube searching has not been configured, needs API key")
		}

		// if these are just words to search for
		// search with the youtube api
		resp, err := searchByKeywords(query.SongQuery)
		if err != nil {
			return []string{}, err
		}

		query.SongQuery = resp.VideoId
	}

	// if its just a regular youtube link
	// pass it along
	title, err := vc.playYoutubeWithId(query)
	return []string{title}, err
}

func (vc *VoiceClient) playYoutubeWithId(s SongRequest) (string, error) {
	info, err := ytdl.GetVideoInfo(s.SongQuery)
	if err != nil {
		return "", err
	}

	fmt.Printf("Queuing Video: %s [%s]\n", info.Title, s.SongQuery)

	audioLink, err := getSortYouTubeAudioLink(info)
	if err != nil {
		return "", err
	}

	s.Title = strings.TrimSpace(info.Title)
	s.SongQuery = audioLink.String()

	vc.queueVideo(s)

	return info.Title, nil
}

func (vc *VoiceClient) playYoutubeList(videos []string, sr SongRequest) ([]string, error) {
	var titleVideos []string

	for _, video := range videos {
		request := SongRequest{
			SongQuery: video,
			ChannelId: sr.ChannelId,
			UserId:    sr.UserId,
		}

		title, err := vc.playYoutubeWithId(request)
		if err != nil {
			log.Println(err)
			continue
		}

		titleVideos = append(titleVideos, title)
	}

	return titleVideos, nil
}

func (vc *VoiceClient) playVideo(url string) {
	encoding := audio.Encode(url, audio.WithDefaults())

	vc.isPlaying = true
	vc.voice.Speaking(true)

	defer func() {
		vc.isPlaying = false
		vc.voice.Speaking(false)
	}()

	for {
		frame, err := encoding.OpusFrame()
		if err != nil {
			if err != io.EOF {
				log.Println(err)
			}
			break
		}

		if vc.stop {
			log.Println("stop")
			break
		}

		if vc.skip {
			log.Println("skip")
			break
		}

		vc.voice.OpusSend <- frame
	}
}

// NowPlaying tracks the voice channel and sends a message about the current song playing.
func (vc *VoiceClient) NowPlaying(sr SongRequest) {
	var msg string

	user, err := vc.discord.User(sr.UserId)
	if err != nil {
		msg = msgNowPlayingAnon(sr.Title)
	} else {
		msg = msgNowPlaying(sr.Title, user)
	}

	if _, err := vc.discord.ChannelMessageSend(sr.ChannelId, msg); err != nil {
		log.Println(err)
	}

	log.Println(msg)
}

func (vc *VoiceClient) processQueue() {
	// if music is currently playing
	// exit early, as another goroutine is (most likely) accessing the queue
	if vc.isPlaying {
		return
	}

	// if !stop was used sometime ago
	// reset it
	if vc.stop {
		vc.stop = false
	}

	// strictly allow one goroutine to dequeue
	vc.mutex.Lock()
	defer vc.mutex.Unlock()

	for {
		if songRequest := vc.queue.Dequeue(); songRequest != nil && !vc.stop {
			sr := songRequest.(SongRequest)

			// send a message that the next song is playing
			// to the original user who requested the song
			vc.NowPlaying(sr)

			// NOTE: this should be blocking
			// as we don't want multiple ffmpeg instances running for every song
			// in the damn queue
			vc.playVideo(sr.SongQuery)
		} else {
			break
		}

		vc.skip = false
	}
}
