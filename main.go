// What it can do
// - Message into slack per tracked PR per channel to alert the wider team
// - Further messages in relation to that PR to be sent as a thread message
// - Daily reminders of all tracked active PRs per channel
// - Notifactions on any new comments made since the last check
// - Notifactions on any new reviewers, or reviewers changing their review
// - Delete all thread messages + parent message once PR is no longer active
// - If a user has requested to track a PR that is already being tracked, it will @ them in slack with any new updates

// Future plans
// - prefix all message with PR ID ---- DONE
// - functionality to collect all PRs tracked per channel and send a morning reminder for remaining active PRs ---- DONE
// - Check if PR is still active here, if not, send message to thread to confirm PR has been completed or abandoned ---- DONE
// - Ensure the same PR can't be added twice ---- DONE
// - If the person has requested a already existing tracked PR, add them to the @ list ---- DONE
// - Update cron to be passed in via var and see if there's a cron to exclude weekends "0 0 9 * * 1-5". ---- DONE
// - Fetch when someone approves the PR ---- added to check if PR has been declined / approved with suggestions also. Needs to check if reviewer has changed their review still ---- DONE
// - Delete first message sent that hosts all thread messages on choice. Have to delete thread messages first ---- (conversation messages also count the parent) ---- DONE
// - Only check unresolved comments. Do we message the comment author that they've had responses? How do we link Azure Devops authers back to slack?
// - A /bump command to push a notifaction to the channel containing all your PRs
// - functionality around comparing comments. If the author has responded to the new comment, dont alert.

// Not Possible Ideas
// - Add a thread message if the PR is ready to be merged ---- NOT POSSIBLE (if its just approvers, the merge status is still 'succeeded' even if not all people have approved)

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/slack-go/slack"
)

const (
	slackVerificationToken = ""
	slackAccessToken       = ""
	personalAccessToken    = ""

	cronTimer = "0 0 9 * * 1-5" // At 9AM, on a weekday

	deleteFirstMessage = false // Delete the first message with all the thread messages once PR is no longer active.
)

var azureDevOpsOrganization string
var azureDevOpsProject string
var repositoryName string

var activeMonitoring = make(map[string]bool) // key: "PRID_channelID", value: true/false
var mutex sync.Mutex                         // Mutex for safe concurrent access to the map

var cronOnce sync.Once
var isCronRunning bool

var interestedUsers = make(map[string][]string) // key: PRID, value: list of user IDs

type Author struct {
	DisplayName string `json:"displayName"`
}

type Comment struct {
	ID          int    `json:"id"`
	Content     string `json:"content"`
	Author      Author `json:"author"`
	CommentType string `json:"commentType"`
}

type CommentThread struct {
	Comments []Comment `json:"comments"`
}

type CommentResponse struct {
	Value []CommentThread `json:"value"`
}

type Reviewer struct {
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
	Vote        int    `json:"vote"`
}

type PullRequest struct {
	ID        int        `json:"pullRequestId"`
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	Reviewers []Reviewer `json:"reviewers"`
}

func fetchCommentsFromAzureDevOps(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID string) ([]Comment, error) {
	azureDevOpsURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s/pullRequests/%s/threads?api-version=6.1",
		azureDevOpsOrganization,
		azureDevOpsProject,
		repositoryName,
		prID,
	)

	req, err := http.NewRequest("GET", azureDevOpsURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(personalAccessToken, "")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var commentResponse CommentResponse
	err = json.Unmarshal(body, &commentResponse)
	if err != nil {
		return nil, err
	}

	var comments []Comment
	for _, thread := range commentResponse.Value {
		for _, comment := range thread.Comments {
			if comment.CommentType != "system" { // Check if CommentType is not "system". System comments count as reviewrs approving / declined / rejecting etc
				comments = append(comments, comment)
			}
		}
	}

	return comments, nil
}

func getPullRequest(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID string) (*PullRequest, error) {
	azureDevOpsURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s/pullRequests/%s?api-version=6.1",
		azureDevOpsOrganization,
		azureDevOpsProject,
		repositoryName,
		prID,
	)

	req, err := http.NewRequest("GET", azureDevOpsURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("", personalAccessToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var pr PullRequest
	err = json.Unmarshal(body, &pr)
	if err != nil {
		return nil, err
	}

	return &pr, nil
}

func getPullRequestStatus(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID string) (status string) {
	pr, err := getPullRequest(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// return error also from other function and check there isn't an error in loop when marking as completed.
	return pr.Status
}

func getPullRequestReviewers(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID string) (reviewers []Reviewer) {
	pr, err := getPullRequest(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// return error also from other function and check there isn't an error in loop when marking as completed.
	return pr.Reviewers
}

func handleSlackSlashCommand(w http.ResponseWriter, r *http.Request) {
	prLink := r.FormValue("text")
	fmt.Println("-------------------\nPR Link received from slack:", prLink)

	// Verify that the request is coming from Slack by checking the token
	token := r.FormValue("token")
	if token != slackVerificationToken {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	fmt.Println("Token Authorised")
	w.WriteHeader(http.StatusOK)

	// Split the URL by "/" to get the parts
	parts := strings.Split(prLink, "/")

	// Find the index of "_git" and use it to split azureDevOpsOrganization, azureDevOpsProject, and repositoryName
	gitIndex := -1
	for i, part := range parts {
		if part == "_git" {
			gitIndex = i
			break
		}
	}

	if gitIndex == -1 || gitIndex+3 >= len(parts) {
		fmt.Println("Invalid URL format")
		return
	}

	azureDevOpsOrganization = parts[gitIndex-2]
	azureDevOpsProject := parts[gitIndex-1]
	repositoryName := parts[gitIndex+1]

	fmt.Println("Organization:", azureDevOpsOrganization)
	fmt.Println("Project:", azureDevOpsProject)
	fmt.Println("Repo:", repositoryName)

	// Extract PR link from Slack command
	prTemplate := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_git/%s/pullrequest",
		azureDevOpsOrganization,
		azureDevOpsProject,
		repositoryName,
	)
	prID := strings.TrimPrefix(prLink, prTemplate)
	prID = strings.TrimSuffix(prID, "/")
	prID = strings.TrimPrefix(prID, "/")

	channelID := r.FormValue("channel_id")
	key := fmt.Sprintf("%s_%s", prID, channelID)
	mutex.Lock()
	if activeMonitoring[key] {
		// PR is already being monitored, add user to interested users if not already added
		userAlreadyInterested := false
		for _, userID := range interestedUsers[prID] {
			if userID == r.FormValue("user_id") {
				userAlreadyInterested = true
				fmt.Println("Submitted from:", r.FormValue("user_name"), "\nChannel: ", r.FormValue("channel_name"), "/", r.FormValue("channel_id"), "\nPR is already being tracked by user...\n-------------------")
				w.Write([]byte("You are already tracking this PR"))
				return
			}
		}

		if !userAlreadyInterested {
			interestedUsers[prID] = append(interestedUsers[prID], r.FormValue("user_id"))
			w.Write([]byte("This PR is already being tracked. You're now interested in this PR, and will be notified of updates."))
		}

		fmt.Println("Submitted from:", r.FormValue("user_name"), "\nChannel: ", r.FormValue("channel_name"), "/", r.FormValue("channel_id"), "\nPR is already being monitored, attempting to add user to monitoring list...\n-------------------")

		mutex.Unlock()
		w.Write([]byte("This PR is already being tracked. You're now interested in this PR, and will be notified of updates.")) // Sending a link to the inital message would be cool
		return
	} else {
		// Add the first requestee as interested in the PR
		userAlreadyInterested := false
		for _, userID := range interestedUsers[prID] {
			if userID == r.FormValue("user_id") {
				userAlreadyInterested = true
			}
		}

		if !userAlreadyInterested {
			interestedUsers[prID] = append(interestedUsers[prID], r.FormValue("user_id"))
		}

		// Mark the PR monitoring as active for this PR and channel
		activeMonitoring[key] = true
		mutex.Unlock()
		fmt.Println("PRID:", prID)

		// Prepare the response message
		pr, err := getPullRequest(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
		if err != nil {
			fmt.Println("Error:", err)
			return
		}

		if getPullRequestStatus(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID) != "active" {
			// Send message back to slack only visible to user to alert the user that PR isnt active.
			fmt.Println("PR isn't active...")
			w.Write([]byte("Please submit an active PR"))
			return
		}

		w.Write([]byte("Processing your request..."))

		firstMessage := fmt.Sprintf("New PR '<%s|*%s*>', created by <@%s>. Tracking PR...",
			prLink,
			pr.Title,
			r.FormValue("user_id"),
		)
		parentMessageTs := sendSlackMessage(slackAccessToken, r.FormValue("channel_id"), firstMessage, "", "", false)
		fmt.Println("Submitted from:", r.FormValue("user_name"), "\nChannel: ", r.FormValue("channel_name"), "/", r.FormValue("channel_id"), "\nParentMessageTs:", parentMessageTs, "\n-------------------")

		// Only start cron if it's not running.
		if !isCronRunning {
			cronOnce.Do(func() {
				startCron(azureDevOpsOrganization, azureDevOpsProject, repositoryName)
			})
		}

		// loop until PR isn't active anymore
		go monitorPr(azureDevOpsOrganization, azureDevOpsProject, repositoryName, parentMessageTs, prID, prLink, r.FormValue("user_id"), r.FormValue("channel_id"))
	}
}

func monitorPr(azureDevOpsOrganization, azureDevOpsProject, repositoryName, parentMessageTs, prID, prLink, userId, channelId string) {
	// Set a timer for each minute
	ticker := time.NewTicker(1 * time.Minute)

	// Setup vars
	uniqueAuthors := make(map[string]bool)
	reviewersApproved := make(map[string]bool)
	reviewersDeclined := make(map[string]bool)
	prefix := fmt.Sprintf("[%s - %s]", channelId, prID)

	var approvedChanged bool
	var declinedChanged bool

	// Fetch comments
	comments, _ := fetchCommentsFromAzureDevOps(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
	currentCommentCount := len(comments)

	// Fetch reviews
	reviews := getPullRequestReviewers(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
	currentReviewsCount := len(reviews)

	for range ticker.C {
		approvedChanged = false
		declinedChanged = false

		interestedUserIDs := interestedUsers[prID]
		mentionText := ""
		for _, userID := range interestedUserIDs {
			mentionText += fmt.Sprintf("<@%s>", userID)
		}

		fmt.Println(prefix, "Checking for changes")
		// Check if PR is still active here, if not, send message to thread to confirm PR has been completed
		if status := getPullRequestStatus(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID); status != "active" {
			mutex.Lock()
			key := fmt.Sprintf("%s_%s", prID, channelId)
			delete(activeMonitoring, key)
			mutex.Unlock()
			fmt.Println(fmt.Sprintf("%s PR isn't active, PR state is %s. Removing from being tracked and sending message to thread", prefix, status))
			if deleteFirstMessage {
				fmt.Println(fmt.Sprintf("%s Deleting messages relating to this tracked PR", prefix))
				// Get all thread messages, delete thread messages and then delete master message
				threadMessages, err := getThreadMessages(slackAccessToken, channelId, parentMessageTs)
				if err != nil {
					fmt.Printf(prefix, "Error retrieving thread messages:", err)
					return
				}
				if err := deleteThreadMessages(slackAccessToken, channelId, threadMessages); err != nil {
					fmt.Printf(prefix, "Error deleting thread messages:", err)
					return
				}

			} else {
				// Send message to thread confirming the new state of the PR to the mention list
				statusMessage := fmt.Sprintf("%s The <%s|PR> has been marked as %s and will no longer be tracked.", mentionText, prLink, status)
				sendSlackMessage(slackAccessToken, channelId, statusMessage, parentMessageTs, "", false)
			}
			break
		}

		newComments, err := fetchCommentsFromAzureDevOps(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
		if err != nil {
			fmt.Println(prefix, "Error fetching comments:", err)
			continue
		}

		// Compare new comments with the existing comments
		newCommentsCount := len(newComments)

		if newCommentsCount > currentCommentCount {
			fmt.Println(fmt.Sprintf("%s New comments found! Current Comments: %d, New Comments: %d", prefix, currentCommentCount, newCommentsCount))
			for i := currentCommentCount; i < newCommentsCount; i++ {
				names := strings.Fields(newComments[i].Author.DisplayName)
				if len(names) > 0 {
					firstName := names[0]
					uniqueAuthors[firstName] = true
				}
			}
			var uniqueAuthorsString string
			for firstName := range uniqueAuthors {
				uniqueAuthorsString += fmt.Sprintf("%s, ", firstName)
			}
			uniqueAuthorsString = strings.TrimSuffix(uniqueAuthorsString, ", ")
			threadMessage := fmt.Sprintf("%s There's *%d* new comment(s) on the <%s|PR> left by %s.",
				mentionText,
				newCommentsCount-currentCommentCount,
				prLink,
				uniqueAuthorsString,
			)
			sendSlackMessage(slackAccessToken, channelId, threadMessage, parentMessageTs, "", false)

			// Update the current comment count with the new count
			currentCommentCount = newCommentsCount
		}

		newReviews := getPullRequestReviewers(azureDevOpsOrganization, azureDevOpsProject, repositoryName, prID)
		newReviewsCount := len(newReviews)

		// Run both newReviews and currentReviews at the same time at each index, compare the vote at each index.
		for i := 0; i < newReviewsCount && i < currentReviewsCount; i++ {
			newReview := newReviews[i]
			currentReview := reviews[i]
			if newReview.UniqueName == currentReview.UniqueName {
				if newReview.Vote != currentReview.Vote {
					if newReview.Vote >= 5 {
						reviewersApproved[newReview.DisplayName] = true
						delete(reviewersDeclined, newReview.DisplayName)
						approvedChanged = true
					} else if newReview.Vote == -10 {
						reviewersDeclined[newReview.DisplayName] = true
						delete(reviewersApproved, newReview.DisplayName)
						declinedChanged = true
					}
				}
			}
		}
		for i := currentReviewsCount; i < newReviewsCount; i++ {
			newReview := newReviews[i]
			// Process the new review without comparing it to any current review
			if newReview.Vote >= 5 { // Approved with suggestions OR Approved
				reviewersApproved[newReview.DisplayName] = true
				approvedChanged = true
			} else if newReview.Vote == -10 { // Declined
				reviewersDeclined[newReview.DisplayName] = true
				declinedChanged = true
			}
		}

		if approvedChanged || declinedChanged {
			approvedReviewers := reviewersToString(reviewersApproved)
			declinedReviewers := reviewersToString(reviewersDeclined)

			var reviewersThreadMessage string
			if approvedReviewers != "" && declinedReviewers != "" {
				reviewersThreadMessage = fmt.Sprintf("%s There's some new reviews on your <%s|PR>. It has been *approved* by %s and *declined* by %s",
					mentionText,
					prLink,
					approvedReviewers,
					declinedReviewers,
				)
			} else if approvedReviewers == "" && declinedReviewers != "" {
				reviewersThreadMessage = fmt.Sprintf("%s There's some new reviews on your <%s|PR>. It has been *declined* by %s",
					mentionText,
					prLink,
					declinedReviewers,
				)
			} else {
				reviewersThreadMessage = fmt.Sprintf("%s There's some new reviews on your <%s|PR>. It has been *approved* by %s",
					mentionText,
					prLink,
					approvedReviewers,
				)
			}
			fmt.Println(prefix, "Some new reviewers found. Approvers:", approvedReviewers, ",Decliners:", declinedReviewers)
			sendSlackMessage(slackAccessToken, channelId, reviewersThreadMessage, parentMessageTs, "", false)
		}
		// Update the current review
		reviews = newReviews
		currentReviewsCount = newReviewsCount

	}
}

func sendSlackMessage(slackAccessToken, channelID, message, messageTs, userId string, postEphemeral bool) (message_ts string) {
	api := slack.New(slackAccessToken)

	if postEphemeral {
		message_ts, err := api.PostEphemeral(channelID, userId, slack.MsgOptionText(message, false))

		if err != nil {
			log.Fatalf("Error sending message: %v", err)
		}
		return message_ts

	} else {
		_, message_ts, err := api.PostMessage(channelID, slack.MsgOptionText(message, false), slack.MsgOptionTS(messageTs))
		if err != nil {
			log.Fatalf("Error sending message: %v", err)
		}
		return message_ts
	}
}

func getThreadMessages(slackAccessToken, channelID, parentTimestamp string) ([]slack.Message, error) {
	api := slack.New(slackAccessToken)

	params := slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: parentTimestamp,
	}
	messages, _, _, err := api.GetConversationReplies(&params)

	if err != nil {
		return nil, err
	}
	return messages, nil
}

func deleteThreadMessages(slackAccessToken, channelID string, messages []slack.Message) error {
	api := slack.New(slackAccessToken)
	for _, msg := range messages {
		_, _, err := api.DeleteMessage(channelID, msg.Timestamp)
		if err != nil {
			return err
		}
	}
	return nil
}

func postActivePRsMessage(activePrs map[string]bool, azureDevOpsOrganization, azureDevOpsProject, repositoryName string) {
	channelMessage := make(map[string]string)
	for key, value := range activePrs {
		if value {
			// Key is in the format "prID_channelID", so split it to get prID and channelID
			parts := strings.Split(key, "_")
			if len(parts) == 2 {
				prID := parts[0]
				channelID := parts[1]
				azureDevOpsURL := fmt.Sprintf(
					"\nhttps://dev.azure.com/%s/%s/_git/%s/pullrequest/%s",
					azureDevOpsOrganization,
					azureDevOpsProject,
					repositoryName,
					prID,
				)

				// Check if the channelID already exists in the channelMessage map
				if existingMessage, ok := channelMessage[channelID]; ok {
					// If the channelID exists, append the new PR URL to the existing message
					channelMessage[channelID] = existingMessage + ", " + azureDevOpsURL
				} else {
					// If the channelID doesn't exist, set the new PR URL as the message
					channelMessage[channelID] = fmt.Sprintf("The follow tracked PRs are still active! Please can we get a review on them today.%s", azureDevOpsURL)
				}
			}
		}
	}
	// Post messages to each channel for a combined message of every PR. Only post if there are still active PRs
	if len(channelMessage) > 0 {
		for channelID, message := range channelMessage {
			sendSlackMessage(slackAccessToken, channelID, message, "", "", false)
			fmt.Println("[GLOBAL] Posting active PRs to", channelID)
		}
	}
}

func startCron(azureDevOpsOrganization, azureDevOpsProject, repositoryName string) {
	cron := cron.New(cron.WithSeconds())
	cron.AddFunc(cronTimer, func() {
		postActivePRsMessage(activeMonitoring, azureDevOpsOrganization, azureDevOpsProject, repositoryName)
	})
	cron.Start()
	fmt.Println("[GLOBAL] Starting cron")
	isCronRunning = true
}

func reviewersToString(reviewers map[string]bool) string {
	reviewerList := make([]string, 0)
	for reviewer := range reviewers {
		reviewerList = append(reviewerList, reviewer)
	}

	return strings.Join(reviewerList, ", ")
}

func main() {
	http.HandleFunc("/slack/pr", handleSlackSlashCommand)
	fmt.Println("[GLOBAL] Server listening on port 80...")
	http.ListenAndServe(":80", nil)
}
