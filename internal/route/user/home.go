// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package user

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/unknwon/com"
	"github.com/unknwon/paginater"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/context"
	"gogs.io/gogs/internal/database"
)

const (
	tmplUserDashboard       = "user/dashboard/dashboard"
	tmplUserDashboardFeeds  = "user/dashboard/feeds"
	tmplUserDashboardIssues = "user/dashboard/issues"
	tmplUserProfile         = "user/profile"
	tmplOrgHome             = "org/home"
)

// getDashboardContextUser finds out dashboard is viewing as which context user.
func getDashboardContextUser(c *context.Context) *database.User {
	ctxUser := c.User
	orgName := c.Params(":org")
	if len(orgName) > 0 {
		// Organization.
		org, err := database.Handle.Users().GetByUsername(c.Req.Context(), orgName)
		if err != nil {
			c.NotFoundOrError(err, "get user by name")
			return nil
		}
		ctxUser = org
	}
	c.Data["ContextUser"] = ctxUser

	orgs, err := database.Handle.Organizations().List(
		c.Req.Context(),
		database.ListOrgsOptions{
			MemberID:              c.User.ID,
			IncludePrivateMembers: true,
		},
	)
	if err != nil {
		c.Error(err, "list organizations")
		return nil
	}
	c.Data["Orgs"] = orgs

	return ctxUser
}

// retrieveFeeds loads feeds from database by given context user.
// The user could be organization so it is not always the logged in user,
// which is why we have to explicitly pass the context user ID.
func retrieveFeeds(c *context.Context, ctxUser *database.User, userID int64, isProfile bool) {
	afterID := c.QueryInt64("after_id")

	var err error
	var actions []*database.Action
	if ctxUser.IsOrganization() {
		actions, err = database.Handle.Actions().ListByOrganization(c.Req.Context(), ctxUser.ID, userID, afterID)
	} else {
		actions, err = database.Handle.Actions().ListByUser(c.Req.Context(), ctxUser.ID, userID, afterID, isProfile)
	}
	if err != nil {
		c.Error(err, "list actions")
		return
	}

	// Check access of private repositories.
	feeds := make([]*database.Action, 0, len(actions))
	unameAvatars := make(map[string]string)
	for _, act := range actions {
		// Cache results to reduce queries.
		_, ok := unameAvatars[act.ActUserName]
		if !ok {
			u, err := database.Handle.Users().GetByUsername(c.Req.Context(), act.ActUserName)
			if err != nil {
				if database.IsErrUserNotExist(err) {
					continue
				}
				c.Error(err, "get user by name")
				return
			}
			unameAvatars[act.ActUserName] = u.AvatarURLPath()
		}

		act.ActAvatar = unameAvatars[act.ActUserName]
		feeds = append(feeds, act)
	}
	c.Data["Feeds"] = feeds
	if len(feeds) > 0 {
		afterID := feeds[len(feeds)-1].ID
		c.Data["AfterID"] = afterID
		c.Header().Set("X-AJAX-URL", fmt.Sprintf("%s?after_id=%d", c.Data["Link"], afterID))
	}
}

func Dashboard(c *context.Context) {
	ctxUser := getDashboardContextUser(c)
	if c.Written() {
		return
	}

	retrieveFeeds(c, ctxUser, c.User.ID, false)
	if c.Written() {
		return
	}

	if c.Req.Header.Get("X-AJAX") == "true" {
		c.Success(tmplUserDashboardFeeds)
		return
	}

	c.Data["Title"] = ctxUser.DisplayName() + " - " + c.Tr("dashboard")
	c.Data["PageIsDashboard"] = true
	c.Data["PageIsNews"] = true

	// Only user can have collaborative repositories.
	if !ctxUser.IsOrganization() {
		collaborateRepos, err := database.Handle.Repositories().GetByCollaboratorID(c.Req.Context(), c.User.ID, conf.UI.User.RepoPagingNum, "updated_unix DESC")
		if err != nil {
			c.Error(err, "get accessible repositories by collaborator")
			return
		} else if err = database.RepositoryList(collaborateRepos).LoadAttributes(); err != nil {
			c.Error(err, "load attributes")
			return
		}
		c.Data["CollaborativeRepos"] = collaborateRepos
	}

	var err error
	var repos, mirrors []*database.Repository
	var repoCount int64
	if ctxUser.IsOrganization() {
		repos, repoCount, err = ctxUser.GetUserRepositories(c.User.ID, 1, conf.UI.User.RepoPagingNum)
		if err != nil {
			c.Error(err, "get user repositories")
			return
		}

		mirrors, err = ctxUser.GetUserMirrorRepositories(c.User.ID)
		if err != nil {
			c.Error(err, "get user mirror repositories")
			return
		}
	} else {
		repos, err = database.GetUserRepositories(
			&database.UserRepoOptions{
				UserID:   ctxUser.ID,
				Private:  true,
				Page:     1,
				PageSize: conf.UI.User.RepoPagingNum,
			},
		)
		if err != nil {
			c.Error(err, "get repositories")
			return
		}
		repoCount = int64(ctxUser.NumRepos)

		mirrors, err = database.GetUserMirrorRepositories(ctxUser.ID)
		if err != nil {
			c.Error(err, "get mirror repositories")
			return
		}
	}
	c.Data["Repos"] = repos
	c.Data["RepoCount"] = repoCount
	c.Data["MaxShowRepoNum"] = conf.UI.User.RepoPagingNum

	if err := database.MirrorRepositoryList(mirrors).LoadAttributes(); err != nil {
		c.Error(err, "load attributes")
		return
	}
	c.Data["MirrorCount"] = len(mirrors)
	c.Data["Mirrors"] = mirrors

	c.Success(tmplUserDashboard)
}

func Issues(c *context.Context) {
	isPullList := c.Params(":type") == "pulls"
	if isPullList {
		c.Data["Title"] = c.Tr("pull_requests")
		c.Data["PageIsPulls"] = true
	} else {
		c.Data["Title"] = c.Tr("issues")
		c.Data["PageIsIssues"] = true
	}

	ctxUser := getDashboardContextUser(c)
	if c.Written() {
		return
	}

	var (
		sortType   = c.Query("sort")
		filterMode = database.FilterModeYourRepos
	)

	// Note: Organization does not have view type and filter mode.
	if !ctxUser.IsOrganization() {
		viewType := c.Query("type")
		types := []string{
			string(database.FilterModeYourRepos),
			string(database.FilterModeAssign),
			string(database.FilterModeCreate),
		}
		if !com.IsSliceContainsStr(types, viewType) {
			viewType = string(database.FilterModeYourRepos)
		}
		filterMode = database.FilterMode(viewType)
	}

	page := c.QueryInt("page")
	if page <= 1 {
		page = 1
	}

	repoID := c.QueryInt64("repo")
	isShowClosed := c.Query("state") == "closed"

	// Get repositories.
	var (
		err         error
		repos       []*database.Repository
		userRepoIDs []int64
		showRepos   = make([]*database.Repository, 0, 10)
	)
	if ctxUser.IsOrganization() {
		repos, _, err = ctxUser.GetUserRepositories(c.User.ID, 1, ctxUser.NumRepos)
		if err != nil {
			c.Error(err, "get repositories")
			return
		}
	} else {
		repos, err = database.GetUserRepositories(
			&database.UserRepoOptions{
				UserID:   ctxUser.ID,
				Private:  true,
				Page:     1,
				PageSize: ctxUser.NumRepos,
			},
		)
		if err != nil {
			c.Error(err, "get repositories")
			return
		}
	}

	userRepoIDs = make([]int64, 0, len(repos))
	for _, repo := range repos {
		userRepoIDs = append(userRepoIDs, repo.ID)

		if filterMode != database.FilterModeYourRepos {
			continue
		}

		if isPullList {
			if isShowClosed && repo.NumClosedPulls == 0 ||
				!isShowClosed && repo.NumOpenPulls == 0 {
				continue
			}
		} else {
			if !repo.EnableIssues || repo.EnableExternalTracker ||
				isShowClosed && repo.NumClosedIssues == 0 ||
				!isShowClosed && repo.NumOpenIssues == 0 {
				continue
			}
		}

		showRepos = append(showRepos, repo)
	}

	// Filter repositories if the page shows issues.
	if !isPullList {
		userRepoIDs, err = database.FilterRepositoryWithIssues(userRepoIDs)
		if err != nil {
			c.Error(err, "filter repositories with issues")
			return
		}
	}

	issueOptions := &database.IssuesOptions{
		RepoID:   repoID,
		Page:     page,
		IsClosed: isShowClosed,
		IsPull:   isPullList,
		SortType: sortType,
	}
	switch filterMode {
	case database.FilterModeYourRepos:
		// Get all issues from repositories from this user.
		if userRepoIDs == nil {
			issueOptions.RepoIDs = []int64{-1}
		} else {
			issueOptions.RepoIDs = userRepoIDs
		}

	case database.FilterModeAssign:
		// Get all issues assigned to this user.
		issueOptions.AssigneeID = ctxUser.ID

	case database.FilterModeCreate:
		// Get all issues created by this user.
		issueOptions.PosterID = ctxUser.ID
	}

	issues, err := database.Issues(issueOptions)
	if err != nil {
		c.Error(err, "list issues")
		return
	}

	if repoID > 0 {
		repo, err := database.GetRepositoryByID(repoID)
		if err != nil {
			c.Error(err, "get repository by ID")
			return
		}

		if err = repo.GetOwner(); err != nil {
			c.Error(err, "get owner")
			return
		}

		// Check if user has access to given repository.
		if !repo.IsOwnedBy(ctxUser.ID) && !repo.HasAccess(ctxUser.ID) {
			c.NotFound()
			return
		}
	}

	for _, issue := range issues {
		if err = issue.Repo.GetOwner(); err != nil {
			c.Error(err, "get owner")
			return
		}
	}

	issueStats := database.GetUserIssueStats(repoID, ctxUser.ID, userRepoIDs, filterMode, isPullList)

	var total int
	if !isShowClosed {
		total = int(issueStats.OpenCount)
	} else {
		total = int(issueStats.ClosedCount)
	}

	c.Data["Issues"] = issues
	c.Data["Repos"] = showRepos
	c.Data["Page"] = paginater.New(total, conf.UI.IssuePagingNum, page, 5)
	c.Data["IssueStats"] = issueStats
	c.Data["ViewType"] = string(filterMode)
	c.Data["SortType"] = sortType
	c.Data["RepoID"] = repoID
	c.Data["IsShowClosed"] = isShowClosed

	if isShowClosed {
		c.Data["State"] = "closed"
	} else {
		c.Data["State"] = "open"
	}

	c.Success(tmplUserDashboardIssues)
}

func ShowSSHKeys(c *context.Context, uid int64) {
	keys, err := database.ListPublicKeys(uid)
	if err != nil {
		c.Error(err, "list public keys")
		return
	}

	var buf bytes.Buffer
	for i := range keys {
		buf.WriteString(keys[i].OmitEmail())
		buf.WriteString("\n")
	}
	c.PlainText(http.StatusOK, buf.String())
}

func showOrgProfile(c *context.Context) {
	c.SetParams(":org", c.Params(":username"))
	context.HandleOrgAssignment(c)
	if c.Written() {
		return
	}

	org := c.Org.Organization
	c.Data["Title"] = org.FullName

	page := c.QueryInt("page")
	if page <= 0 {
		page = 1
	}

	var (
		repos []*database.Repository
		count int64
		err   error
	)
	if c.IsLogged && !c.User.IsAdmin {
		repos, count, err = org.GetUserRepositories(c.User.ID, page, conf.UI.User.RepoPagingNum)
		if err != nil {
			c.Error(err, "get user repositories")
			return
		}
		c.Data["Repos"] = repos
	} else {
		showPrivate := c.IsLogged && c.User.IsAdmin
		repos, err = database.GetUserRepositories(&database.UserRepoOptions{
			UserID:   org.ID,
			Private:  showPrivate,
			Page:     page,
			PageSize: conf.UI.User.RepoPagingNum,
		})
		if err != nil {
			c.Error(err, "get user repositories")
			return
		}
		c.Data["Repos"] = repos
		count = database.CountUserRepositories(org.ID, showPrivate)
	}
	c.Data["Page"] = paginater.New(int(count), conf.UI.User.RepoPagingNum, page, 5)

	if err := org.GetMembers(12); err != nil {
		c.Error(err, "get members")
		return
	}
	c.Data["Members"] = org.Members

	c.Data["Teams"] = org.Teams

	c.Success(tmplOrgHome)
}

func Email2User(c *context.Context) {
	u, err := database.Handle.Users().GetByEmail(c.Req.Context(), c.Query("email"))
	if err != nil {
		c.NotFoundOrError(err, "get user by email")
		return
	}
	c.Redirect(conf.Server.Subpath + "/user/" + u.Name)
}
