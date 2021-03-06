package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andygrunwald/go-jira"
	"github.com/joushou/qp"
	"github.com/joushou/qptools/fileserver/trees"
)

type WorklogView struct {
	issueNo string
	worklog string
}

func (wv *WorklogView) Walk(jc *Client, file string) (trees.File, error) {
	w, err := GetSpecificWorklogForIssue(jc, wv.issueNo, wv.worklog)
	if err != nil {
		return nil, err
	}

	sf := trees.NewSyntheticFile(file, 0555, "jira", "jira")
	switch file {
	case "comment":
		sf.SetContent([]byte(w.Comment + "\n"))
	case "author":
		sf.SetContent([]byte(w.Author.Name + "\n"))
	case "time":
		t := time.Duration(w.TimeSpentSeconds) * time.Second
		sf.SetContent([]byte(t.String() + "\n"))
	case "started":
		sf.SetContent([]byte(time.Time(*w.Started).String() + "\n"))
	default:
		return nil, nil
	}

	return sf, nil
}

func (wv *WorklogView) List(jc *Client) ([]qp.Stat, error) {
	return StringsToStats([]string{"comment", "author", "time", "started"}, 0555, "jira", "jira"), nil
}

type IssueWorklogView struct {
	issueNo string
}

func (iwv *IssueWorklogView) Walk(jc *Client, file string) (trees.File, error) {
	w, err := GetWorklogForIssue(jc, iwv.issueNo)
	if err != nil {
		return nil, err
	}

	for _, wr := range w.Worklogs {
		if wr.ID == file {
			return NewJiraDir(file,
				0555|qp.DMDIR,
				"jira",
				"jira",
				jc,
				&WorklogView{issueNo: iwv.issueNo, worklog: file})
		}
	}

	return nil, nil
}

func (iwv *IssueWorklogView) List(jc *Client) ([]qp.Stat, error) {
	w, err := GetWorklogForIssue(jc, iwv.issueNo)
	if err != nil {
		return nil, err
	}

	var s []string
	for _, wr := range w.Worklogs {
		s = append(s, wr.ID)
	}

	return StringsToStats(s, 0555|qp.DMDIR, "jira", "jira"), nil
}

type CommentView struct {
	issueNo string
	comment string
}

func (cw *CommentView) Walk(jc *Client, file string) (trees.File, error) {
	if !StringExistsInSets(file, []string{"author", "comment", "updated", "created"}) {
		return nil, nil
	}

	cmt, err := GetComment(jc, cw.issueNo, cw.comment)
	if err != nil {
		return nil, err
	}

	var cnt []byte
	writable := false
	forceTrunc := true
	switch file {
	case "author":
		cnt = []byte(cmt.Author.Name + "\n")
	case "comment":
		cnt = []byte(cmt.Body)
		forceTrunc = false
		writable = true
	case "updated":
		cnt = []byte(cmt.Updated + "\n")
	case "created":
		cnt = []byte(cmt.Created + "\n")
	}
	var perm qp.FileMode
	if writable {
		perm = 0777
	} else {
		perm = 0555
	}
	sf := trees.NewSyntheticFile(file, perm, "jira", "jira")
	sf.SetContent(cnt)

	onClose := func() error {
		sf.RLock()
		str := string(sf.Content)
		sf.RUnlock()

		switch file {
		case "comment":
			return SetComment(jc, cw.issueNo, cw.comment, str)
		}
		return nil
	}

	if writable {
		cs := NewCloseSaver(sf, onClose)
		cs.forceTrunc = forceTrunc
		return cs, nil
	}

	return sf, nil
}

func (cw *CommentView) List(jc *Client) ([]qp.Stat, error) {
	a := StringsToStats([]string{"comment"}, 0777, "jira", "jira")
	b := StringsToStats([]string{"author", "updated", "created"}, 0555, "jira", "jira")
	return append(a, b...), nil
}

type IssueCommentView struct {
	issueNo string
}

func (icv *IssueCommentView) Walk(jc *Client, file string) (trees.File, error) {
	switch file {
	case "comment":
		sf := trees.NewSyntheticFile(file, 0777, "jira", "jira")
		onClose := func() error {
			sf.Lock()
			body := string(sf.Content)
			sf.Unlock()

			return AddComment(jc, icv.issueNo, body)
		}
		return NewCloseSaver(sf, onClose), nil
	default:
		_, err := GetComment(jc, icv.issueNo, file)
		if err != nil {
			return nil, err
		}
		cv := &CommentView{issueNo: icv.issueNo, comment: file}
		return NewJiraDir(file, 0777|qp.DMDIR, "jira", "jira", jc, cv)
	}
}

func (icv *IssueCommentView) List(jc *Client) ([]qp.Stat, error) {
	strs, err := GetCommentsForIssue(jc, icv.issueNo)
	if err != nil {
		return nil, err
	}

	a := StringsToStats(strs, 0777|qp.DMDIR, "jira", "jira")
	b := StringsToStats([]string{"comment"}, 0777, "jira", "jira")

	return append(a, b...), nil
}

func (icv *IssueCommentView) Remove(jc *Client, name string) error {
	switch name {
	case "comment":
		return trees.ErrPermissionDenied
	default:
		return RemoveComment(jc, icv.issueNo, name)
	}
}

type IssueView struct {
	project string
	issueNo string

	issueLock sync.Mutex
	newIssue  bool
	values    map[string]string
}

func (iw *IssueView) normalFiles() (files, dirs []string) {
	files = []string{"assignee", "creator", "ctl", "description", "type", "key", "reporter", "status",
		"summary", "labels", "transition", "priority", "resolution", "raw", "progress", "links", "components",
		"project"}
	dirs = []string{"comments", "worklog"}
	return
}

func (iw *IssueView) newFiles() (files, dirs []string) {
	files = []string{"ctl", "description", "type", "summary", "project"}
	return
}

func (iw *IssueView) newWalk(jc *Client, file string) (trees.File, error) {
	files, dirs := iw.newFiles()
	if !StringExistsInSets(file, files, dirs) {
		return nil, nil
	}

	switch file {
	case "ctl":
		cmds := map[string]func([]string) error{
			"commit": func(args []string) error {
				var issuetype, summary, description, project string

				iw.issueLock.Lock()
				isNew := iw.newIssue
				if iw.values != nil {
					issuetype = strings.Replace(string(iw.values["type"]), "\n", "", -1)
					summary = strings.Replace(string(iw.values["summary"]), "\n", "", -1)
					description = string(iw.values["description"])
					project = strings.Replace(string(iw.values["project"]), "\n", "", -1)
				}
				iw.issueLock.Unlock()

				if project == "" && iw.project != "" {
					project = iw.project
				}

				if !isNew {
					return errors.New("issue already committed")
				}

				issue := jira.Issue{
					Fields: &jira.IssueFields{
						Type: jira.IssueType{
							Name: issuetype,
						},
						Project: jira.Project{
							Key: project,
						},
						Summary:     summary,
						Description: description,
					},
				}

				key, err := CreateIssue(jc, &issue)
				if err != nil {
					log.Printf("Create failed: %v", err)
					return err
				}

				iw.issueLock.Lock()
				iw.issueNo = key
				iw.project = project
				iw.newIssue = false
				iw.issueLock.Unlock()
				return nil
			},
		}
		return NewCommandFile("ctl", 0777, "jira", "jira", cmds), nil
	default:
		sf := trees.NewSyntheticFile(file, 0777, "jira", "jira")
		iw.issueLock.Lock()
		defer iw.issueLock.Unlock()

		if iw.values == nil {
			iw.values = make(map[string]string)
		}

		value := iw.values[file]

		sf.SetContent([]byte(value))

		onClose := func() error {
			iw.issueLock.Lock()
			defer iw.issueLock.Unlock()

			iw.values[file] = string(sf.Content)
			return nil
		}

		return NewCloseSaver(sf, onClose), nil
	}

}

func renderIssueLink(l *jira.IssueLink, key string) string {
	switch {
	case l.OutwardIssue != nil:
		return fmt.Sprintf("%s %s %s", key, l.OutwardIssue.Key, l.Type.Name)
	case l.InwardIssue != nil:
		return fmt.Sprintf("%s %s %s", l.InwardIssue.Key, key, l.Type.Name)
	default:
		return ""
	}
}

func (iw *IssueView) normalWalk(jc *Client, file string) (trees.File, error) {
	files, dirs := iw.normalFiles()
	if !StringExistsInSets(file, files, dirs) {
		return nil, nil
	}

	issue, err := GetIssue(jc, iw.issueNo)
	if err != nil {
		return nil, err
	}

	forceTrunc := true
	writable := true

	var cnt []byte
	switch file {
	case "assignee":
		if issue.Fields != nil && issue.Fields.Assignee != nil {
			cnt = []byte(issue.Fields.Assignee.Name + "\n")
		}
	case "reporter":
		if issue.Fields != nil && issue.Fields.Reporter != nil {
			cnt = []byte(issue.Fields.Reporter.Name + "\n")
		}
	case "creator":
		if issue.Fields != nil && issue.Fields.Creator != nil {
			cnt = []byte(issue.Fields.Creator.Name + "\n")
		}
	case "summary":
		if issue.Fields != nil {
			cnt = []byte(issue.Fields.Summary + "\n")
		}
		forceTrunc = false
	case "description":
		if issue.Fields != nil {
			cnt = []byte(issue.Fields.Description + "\n")
		}
		forceTrunc = false
	case "type":
		if issue.Fields != nil {
			cnt = []byte(issue.Fields.Type.Name + "\n")
		}
	case "status":
		if issue.Fields != nil && issue.Fields.Status != nil {
			cnt = []byte(issue.Fields.Status.Name + "\n")
		}
	case "priority":
		if issue.Fields != nil && issue.Fields.Priority != nil {
			cnt = []byte(issue.Fields.Priority.Name + "\n")
		}
	case "resolution":
		if issue.Fields != nil && issue.Fields.Resolution != nil {
			cnt = []byte(issue.Fields.Resolution.Name + "\n")
		}
	case "progress":
		if issue.Fields != nil && issue.Fields.Progress != nil {
			p := time.Duration(issue.Fields.Progress.Progress) * time.Second
			t := time.Duration(issue.Fields.Progress.Total) * time.Second
			r := t - p
			cnt = []byte(fmt.Sprintf("Progress: %v, Remaining: %v, Total: %v\n", p, r, t))
		}
		writable = false
	case "project":
		if issue.Fields != nil {
			cnt = []byte(issue.Fields.Project.Key + "\n")
		}
		writable = false
	case "key":
		cnt = []byte(issue.Key + "\n")
		writable = false
	case "components":
		if issue.Fields != nil {
			var s string
			for _, comp := range issue.Fields.Components {
				s += comp.Name + "\n"
			}
			cnt = []byte(s)
		}
		forceTrunc = false
	case "labels":
		if issue.Fields != nil {
			var s string
			for _, lbl := range issue.Fields.Labels {
				s += lbl + "\n"
			}
			cnt = []byte(s)
		}
		forceTrunc = false
	case "transition":
		trs, err := GetTransitionsForIssue(jc, issue.Key)
		if err != nil {
			log.Printf("Could not get transitions for issue %s: %v", issue.Key, err)
			return nil, err
		}

		var s string
		for _, tr := range trs {
			s += tr.Name + "\n"
		}
		cnt = []byte(s)
	case "links":
		var s string
		if issue.Fields != nil {
			for _, l := range issue.Fields.IssueLinks {
				s += renderIssueLink(l, issue.Key) + "\n"
			}
			cnt = []byte(s)
		}
		forceTrunc = false
	case "comments":
		return NewJiraDir(file,
			0555|qp.DMDIR,
			"jira",
			"jira",
			jc,
			&IssueCommentView{issueNo: iw.issueNo})
	case "worklog":
		return NewJiraDir(file,
			0555|qp.DMDIR,
			"jira",
			"jira",
			jc,
			&IssueWorklogView{issueNo: iw.issueNo})
	case "raw":
		b, err := json.MarshalIndent(issue, "", "	")
		if err != nil {
			return nil, err
		}
		cnt = b
	case "ctl":
		cmds := map[string]func([]string) error{
			"delete": func(args []string) error {
				return DeleteIssue(jc, issue.Key)
			},
		}
		return NewCommandFile("ctl", 0777, "jira", "jira", cmds), nil
	}

	var perm qp.FileMode
	if writable {
		perm = 0777
	} else {
		perm = 0555
	}

	sf := trees.NewSyntheticFile(file, perm, "jira", "jira")
	sf.SetContent(cnt)

	onClose := func() error {
		switch file {
		case "raw":
			sf.RLock()
			defer sf.RUnlock()
			return SetIssueRaw(jc, issue.Key, sf.Content)
		case "links":
			cur := make(map[string]string)
			for _, l := range issue.Fields.IssueLinks {
				cur[renderIssueLink(l, issue.Key)] = l.ID
			}

			sf.RLock()
			str := string(sf.Content)
			sf.RUnlock()

			// Figure out which issue links are new, and which are old.
			var new []string
			input := strings.Split(str, "\n")
			for _, s := range input {
				if s == "" {
					continue
				}
				if _, exists := cur[s]; !exists {
					new = append(new, s)
				} else {
					delete(cur, s)
				}
			}

			// Delete the remaining old issue links
			for k, v := range cur {
				err := DeleteIssueLink(jc, v)
				if err != nil {
					log.Printf("Could not delete issue link %s (%s): %v", v, k, err)
				}
			}

			for _, k := range new {
				args := strings.Split(k, " ")
				if len(args) != 3 {
					continue
				}
				if args[0] != issue.Key && args[1] != issue.Key {
					continue
				}
				err := LinkIssues(jc, args[0], args[1], args[2])
				if err != nil {
					log.Printf("Could not create issue link (%s): %v", k, err)
				}
			}

			return nil
		case "transition":
			sf.RLock()
			str := string(sf.Content)
			sf.RUnlock()
			str = strings.Replace(str, "\n", "", -1)

			return TransitionIssue(jc, issue.Key, str)

		case "status":
			sf.RLock()
			str := string(sf.Content)
			sf.RUnlock()
			str = strings.Replace(str, "\n", "", -1)

			issue, err := GetIssue(jc, iw.issueNo)
			if err != nil {
				log.Printf("Could not fetch issue: %v", err)
				return err
			}
			if issue.Fields == nil {
				log.Printf("Issue missing fields")
				return errors.New("oops")
			}
			if issue.Fields.Status == nil {
				log.Printf("Issue missing status")
				return errors.New("oops2")
			}

			wg, err := BuildWorkflow2(jc, iw.project, issue.Fields.Type.ID)
			if err != nil {
				log.Printf("Could not build workflow: %v", err)
				return err
			}

			p, err := wg.Path(issue.Fields.Status.Name, str, 500)
			if err != nil {
				log.Printf("Could not find path: %v", err)
				log.Printf("Workflow: \n%s\n", wg.Dump())
				return err
			}

			log.Printf("Workflow path: %s", strings.Join(p, ", "))

			for _, s := range p {
				err = TransitionIssue(jc, issue.Key, s)
				if err != nil {
					log.Printf("Could not transition issue: %v", err)
					return err
				}
			}

			return nil

		default:
			sf.RLock()
			str := string(sf.Content)
			sf.RUnlock()
			switch file {
			case "description", "labels", "components":
			default:
				str = strings.Replace(str, "\n", "", -1)
			}
			return SetFieldInIssue(jc, issue.Key, file, str)
		}
	}

	if writable {
		cs := NewCloseSaver(sf, onClose)
		cs.forceTrunc = forceTrunc
		return cs, nil
	}

	return sf, nil
}

func (iw *IssueView) Walk(jc *Client, file string) (trees.File, error) {
	iw.issueLock.Lock()
	isNew := iw.newIssue
	iw.issueLock.Unlock()

	if isNew {
		return iw.newWalk(jc, file)
	} else {
		return iw.normalWalk(jc, file)
	}
}

func (iw *IssueView) List(jc *Client) ([]qp.Stat, error) {
	iw.issueLock.Lock()
	isNew := iw.newIssue
	iw.issueLock.Unlock()

	var files, dirs []string
	if isNew {
		files, dirs = iw.newFiles()
	} else {
		files, dirs = iw.normalFiles()
	}
	var stats []qp.Stat

	stats = append(stats, StringsToStats(files, 0777, "jira", "jira")...)
	stats = append(stats, StringsToStats(dirs, 0777|qp.DMDIR, "jira", "jira")...)

	return stats, nil
}

type SearchView struct {
	query      string
	resultLock sync.Mutex
	results    []string
}

func (sw *SearchView) search(jc *Client) error {
	keys, err := GetKeysForSearch(jc, sw.query, jc.maxlisting)
	if err != nil {
		return err
	}

	sw.resultLock.Lock()
	sw.results = keys
	sw.resultLock.Unlock()
	return nil
}

func (sw *SearchView) Walk(jc *Client, file string) (trees.File, error) {
	sw.resultLock.Lock()
	keys := sw.results
	sw.resultLock.Unlock()

	if !StringExistsInSets(file, keys) {
		return nil, trees.ErrNoSuchFile
	}

	issue, err := GetIssue(jc, file)
	if err != nil {
		return nil, err
	}

	if issue.Fields == nil {
		return nil, errors.New("nil fields in issue")
	}

	iw := &IssueView{
		project: issue.Fields.Project.Key,
		issueNo: issue.Key,
	}

	return NewJiraDir(file, 0555|qp.DMDIR, "jira", "jira", jc, iw)
}

func (sw *SearchView) List(jc *Client) ([]qp.Stat, error) {
	if err := sw.search(jc); err != nil {
		return nil, err
	}

	sw.resultLock.Lock()
	keys := sw.results
	sw.resultLock.Unlock()

	return StringsToStats(keys, 0555|qp.DMDIR, "jira", "jira"), nil
}

type ProjectIssuesView struct {
	project string
}

func (piw *ProjectIssuesView) Walk(jc *Client, issueNo string) (trees.File, error) {
	iw := &IssueView{
		project: piw.project,
	}

	if issueNo == "new" {
		iw.newIssue = true
	} else {
		// Check if the thing is a valid issue number.
		if _, err := strconv.ParseUint(issueNo, 10, 64); err != nil {
			return nil, nil
		}

		issueKey := fmt.Sprintf("%s-%s", piw.project, issueNo)
		_, err := GetIssue(jc, issueKey)
		if err != nil {
			log.Printf("Could not get issue details: %v", err)
			return nil, err
		}
		iw.issueNo = issueKey
	}

	return NewJiraDir(issueNo, 0555|qp.DMDIR, "jira", "jira", jc, iw)
}

func (piw *ProjectIssuesView) List(jc *Client) ([]qp.Stat, error) {
	keys, err := GetKeysForNIssuesInProject(jc, piw.project, jc.maxlisting)
	if err != nil {
		log.Printf("Could not generate issue list: %v", err)
		return nil, err
	}

	keys = append(keys, "new")
	return StringsToStats(keys, 0555|qp.DMDIR, "jira", "jira"), nil
}

type ProjectView struct {
	project string
}

func (pw *ProjectView) Walk(jc *Client, file string) (trees.File, error) {
	switch file {
	case "issues":
		piw := &ProjectIssuesView{project: pw.project}
		return NewJiraDir(file, 0555|qp.DMDIR, "jira", "jira", jc, piw)
	case "components":
		project, err := GetProject(jc, pw.project)
		if err != nil {
			return nil, err
		}

		var components string
		for _, c := range project.Components {
			components += c.Name + "\n"
		}

		sf := trees.NewSyntheticFile(file, 0555, "jira", "jira")
		sf.SetContent([]byte(components))
		return sf, nil
	case "issuetypes":
		project, err := GetProject(jc, pw.project)
		if err != nil {
			return nil, err
		}

		var issuetypes string
		for _, tp := range project.IssueTypes {
			issuetypes += tp.Name + "\n"
		}
		sf := trees.NewSyntheticFile(file, 0555, "jira", "jira")
		sf.SetContent([]byte(issuetypes))
		return sf, nil
	case "raw":
		project, err := GetProject(jc, pw.project)
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(project, "", "	")
		if err != nil {
			return nil, err
		}
		sf := trees.NewSyntheticFile(file, 0555, "jira", "jira")
		sf.SetContent([]byte(b))
		return sf, nil
	default:
		return nil, nil
	}
}

func (pw *ProjectView) List(jc *Client) ([]qp.Stat, error) {
	return StringsToStats([]string{"issues", "issuetypes", "components", "raw"}, 0555|qp.DMDIR, "jira", "jira"), nil
}

type AllProjectsView struct{}

func (apw *AllProjectsView) Walk(jc *Client, projectName string) (trees.File, error) {
	projectName = strings.ToUpper(projectName)
	projects, err := GetProjects(jc)
	if err != nil {
		log.Printf("Could not generate project list: %v", err)
		return nil, err
	}

	pw := &ProjectView{project: projectName}

	for _, project := range projects {
		if project.Key == projectName {
			return NewJiraDir(projectName, 0555|qp.DMDIR, "jira", "jira", jc, pw)
		}
	}

	return nil, nil
}

func (apw *AllProjectsView) List(jc *Client) ([]qp.Stat, error) {
	projects, err := GetProjects(jc)
	if err != nil {
		log.Printf("Could not generate project list: %v", err)
		return nil, err
	}

	var strs []string
	for _, p := range projects {
		strs = append(strs, p.Key)
	}

	return StringsToStats(strs, 0555|qp.DMDIR, "jira", "jira"), nil
}

type AllIssuesView struct{}

func (aiv *AllIssuesView) Walk(jc *Client, issueKey string) (trees.File, error) {
	iw := &IssueView{}

	if issueKey == "new" {
		iw.newIssue = true
	} else if issueKey == "help" {
		message := `new/: New is a folder that creates a new skeleton issue when entered. It only contains a minimal set of files necessary to create the issue. Once all fields have been filled out, writing "commit" to the ctl file will cause the issue to be created. The issue folder will change to be that of a created issue, with all files available. Read the "key" file to figure out what issue key your issue received.
ABC-1/: A folder containing information for ticket '1' in project 'ABC'.
ABC-1/comments/: A folder containing comments for the issue. Writing to the comment file creates a new comment. Writing to an existing comment changes it. This structure may change in the future.
ABC-1/components: A list of components this issue applies to. Writable. Note that the component names are case sensitive, and must be match an existing component for the project.
ABC-1/ctl: A command file. On a new issue, the only accepted command is "commit", which creates the issue with the provided parameters. For existing issues, the only accepted command is "delete". In the future, more commands may be made available for things that map poorly to files.
ABC-1/links: Issue links in the form of "INWARD-ISSUE OUTWARD-ISSUE RELATIONSHIP", such as "ABC-1 ABC-2 Blocks". Writable.
ABC-1/raw: The raw JSON issue object. Writable. Expects the written data to be JSON, and the write will be pushed as an issue update.
ABC-1/status: When writing to the status file, jirafs will fetch the relevant workflow graph and trace the shortest path from the current status to the requested status, issuing the necessary transitions in order.
ABC-1/transition: A list of currently possible transitions. Writing to the file executes the transition. See status for a more convenient way of changing issue status.

For deeper structural representation under this hierarchy, cat 'structure'.
`
		sf := trees.NewSyntheticFile(issueKey, 0555, "jira", "jira")
		sf.SetContent([]byte(message))
		return sf, nil
	} else if issueKey == "structure" {
		message := `new/
	 ctl
	 description
	 project
	 summary
	 type
  ABC-1/
	 assignee
	 comments/
		1/
			author
			updated
			created
			comment
		2/
			...
		...
		comment
	 components
	 creator
	 ctl
	 description
	 key
	 labels
	 links
	 priority
	 progress
	 project
	 raw
	 reporter
	 resolution
	 status
	 summary
	 transition
	 type
	 worklog/
		1/
			author
			started
			time
			comment
		...
  ABC-2/
	 ...
  ...
`
		sf := trees.NewSyntheticFile(issueKey, 0555, "jira", "jira")
		sf.SetContent([]byte(message))
		return sf, nil
	} else {
		s := strings.Split(strings.ToUpper(issueKey), "-")
		if len(s) != 2 {
			return nil, nil
		}

		if _, err := strconv.ParseUint(s[1], 10, 64); err != nil {
			return nil, nil
		}

		issue, err := GetIssue(jc, issueKey)
		if err != nil {
			log.Printf("Could not get issue details: %v", err)
			return nil, err
		}
		if issue.Fields == nil {
			return nil, errors.New("no fields")
		}

		iw.issueNo = issueKey
		iw.project = issue.Fields.Project.Key
	}

	return NewJiraDir(issueKey, 0555|qp.DMDIR, "jira", "jira", jc, iw)
}

func (aiv *AllIssuesView) List(jc *Client) ([]qp.Stat, error) {
	keys, err := GetKeysForSearch(jc, "", jc.maxlisting)
	if err != nil {
		log.Printf("Could not generate issue list: %v", err)
		return nil, err
	}

	keys = append(keys, "new")
	issues := StringsToStats(keys, 0555|qp.DMDIR, "jira", "jira")
	help := StringsToStats([]string{"help", "structure"}, 055, "jira", "jira")
	return append(issues, help...), nil
}

type JiraView struct {
	searchLock sync.Mutex
	searches   map[string]*SearchView
}

func (jw *JiraView) Walk(jc *Client, file string) (trees.File, error) {
	jw.searchLock.Lock()
	defer jw.searchLock.Unlock()
	if jw.searches == nil {
		jw.searches = make(map[string]*SearchView)
	}

	switch file {
	case "ctl":
		cmds := map[string]func([]string) error{
			"search": func(args []string) error {
				if len(args) < 2 {
					return errors.New("query missing")
				}

				sw := &SearchView{query: strings.Join(args[1:], " ")}
				if err := sw.search(jc); err != nil {
					return err
				}

				jw.searchLock.Lock()
				jw.searches[args[0]] = sw
				jw.searchLock.Unlock()
				return nil
			},
			"pass-login": func(args []string) error {
				if len(args) == 2 {
					jc.user = args[0]
					jc.pass = args[1]
				}
				return nil
			},
			"set": func(args []string) error {
				if len(args) != 2 {
					return errors.New("invalid arguments")
				}
				switch args[0] {
				case "max-listing":
					mi, err := strconv.ParseInt(args[1], 10, 64)
					if err != nil {
						return err
					}
					jc.maxlisting = int(mi)
					return nil
				default:
					return errors.New("unknown variable")
				}
			},
		}
		return NewCommandFile("ctl", 0777, "jira", "jira", cmds), nil
	case "projects":
		return NewJiraDir(file, 0555|qp.DMDIR, "jira", "jira", jc, &AllProjectsView{})
	case "issues":
		return NewJiraDir(file, 0555|qp.DMDIR, "jira", "jira", jc, &AllIssuesView{})
	case "structure":
		message := `
/
	ctl
	projects/
	  ABC/
		 components
		 issuetypes
		 issues/
			1/ # ABC-1
				...
			...

	  DEF/
		 ...
	  ...
	issues/
	  new/
		 ctl
		 description
		 project
		 summary
		 type
	  ABC-1/
		 assignee
		 comments/
			1/
				author
				updated
				created
				comment
			2/
				...
			...
			comment
		 components
		 creator
		 ctl
		 description
		 key
		 labels
		 links
		 priority
		 progress
		 project
		 raw
		 reporter
		 resolution
		 status
		 summary
		 transition
		 type
		 worklog/
			1/
				author
				started
				time
				comment
			...
	  ABC-2/
		 ...
	  ...
`
		sf := trees.NewSyntheticFile(file, 0555, "jira", "jira")
		sf.SetContent([]byte(message))
		return sf, nil
	case "help":
		message := `ctl: A global control file. It supports the following commands:
	* search search_name JQL
		If successful, a folder named search_name will appear at the jirafs root. ls'ing in the folder updates the search. The search does not update when simply trying to access an issue in order to avoid significant performance issues.
	* pass-login
		Re-issue a username/password login using the initially provided credentials.
	* set name val
		Sets jirafs variables. Currently, max-listing is the only variable, which expects an integer.
projects/: Directory listing of projects.
issues/: Directory listing of issues

For deeper structural representation, cat 'structure'
`
		sf := trees.NewSyntheticFile(file, 0555, "jira", "jira")
		sf.SetContent([]byte(message))
		return sf, nil
	default:
		search, exists := jw.searches[file]

		if !exists {
			return nil, nil
		}

		return NewJiraDir(file, 0555|qp.DMDIR, "jira", "jira", jc, search)
	}
}

func (jw *JiraView) List(jc *Client) ([]qp.Stat, error) {
	jw.searchLock.Lock()
	defer jw.searchLock.Unlock()
	if jw.searches == nil {
		jw.searches = make(map[string]*SearchView)
	}

	var strs []string
	for k := range jw.searches {
		strs = append(strs, k)
	}

	a := StringsToStats([]string{"projects", "issues"}, 0555|qp.DMDIR, "jira", "jira")
	b := StringsToStats([]string{"ctl"}, 0777, "jira", "jira")
	c := StringsToStats([]string{"help", "structure"}, 0555, "jira", "jira")
	d := StringsToStats(strs, 0777|qp.DMDIR, "jira", "jira")
	return append(append(append(a, b...), c...), d...), nil
}

func (jw *JiraView) Remove(jc *Client, file string) error {
	switch file {
	case "ctl", "projects", "issues", "structure", "help":
		return trees.ErrPermissionDenied
	default:
		jw.searchLock.Lock()
		defer jw.searchLock.Unlock()
		if jw.searches == nil {
			jw.searches = make(map[string]*SearchView)
		}

		if _, exists := jw.searches[file]; exists {
			delete(jw.searches, file)
			return nil
		}

		return trees.ErrNoSuchFile
	}
}
