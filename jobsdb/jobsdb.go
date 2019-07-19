/*
Implementation of JobsDB for keeping track of jobs (type JobT) and job status
(type JobStatusT). Jobs are stored in jobs_%d table while job status is stored
in job_status_%d table. Each such table pair (e.g. jobs_1, job_status_1) is called
a dataset (type dataSetT). After a dataset grows beyond a size, a new dataset is
created and jobs are written to a new dataset. When most of the jobs from a dataset
have been processed, we migrate the remaining jobs to a new intermediate
dataset and delete the old dataset. The range of job ids in a dataset are tracked
via the dataSetRangeT struct

The key reason for choosing this structure is to avoid costly DELETE and UPDATE
operations in DB. Instead, we just use WRITE (append)  and DELETE TABLE (deleting a file)
operations which are fast.
Also, keeping each dataset small (enough to cache in memory) ensures that reads are
mostly serviced from memory cache.
*/

package jobsdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	uuid "github.com/satori/go.uuid"
)

/*
JobStatusT is used for storing status of the job. It is
the responsibility of the user of this module to set appropriate
job status. State can be one of
ENUM waiting, executing, succeeded, waiting_retry,  failed, aborted
*/
type JobStatusT struct {
	JobID         int64
	JobState      string //ENUM waiting, executing, succeeded, waiting_retry,  failed, aborted
	AttemptNum    int
	ExecTime      time.Time
	RetryTime     time.Time
	ErrorCode     string
	ErrorResponse json.RawMessage
}

/*
JobT is the basic type for creating jobs. The JobID is generated
by the system and LastJobStatus is populated when reading a processed
job  while rest should be set by the user.
*/
type JobT struct {
	UUID          uuid.UUID
	JobID         int64
	CreatedAt     time.Time
	ExpireAt      time.Time
	CustomVal     string
	EventPayload  json.RawMessage
	LastJobStatus JobStatusT
}

//The struct fields need to be exposed to JSON package
type dataSetT struct {
	JobTable       string `json:"job"`
	JobStatusTable string `json:"status"`
	Index          string `json:"index"`
}

type dataSetRangeT struct {
	minJobID int64
	maxJobID int64
	ds       dataSetT
}

/*
HandleT is the main type implementing the database for implementing
jobs. The caller must call the SetUp function on a HandleT object
*/
type HandleT struct {
	dbHandle           *sql.DB
	tablePrefix        string
	datasetList        []dataSetT
	datasetRangeList   []dataSetRangeT
	dsListLock         sync.RWMutex
	dsMigrationLock    sync.RWMutex
	dsRetentionPeriod  time.Duration
	dsEmptyResultCache map[dataSetT]map[string]map[string]bool
	dsCacheLock        sync.Mutex
}

//The struct which is written to the journal
type journalOpPayloadT struct {
	From []dataSetT `json:"from"`
	To   dataSetT   `json:"to"`
}

//Some helper functions
func (jd *HandleT) assertError(err error) {
	if err != nil {
		debug.SetTraceback("all")
		debug.PrintStack()
		jd.printLists(true)
		fmt.Println(jd.dsEmptyResultCache)
		panic(err)
	}
}

func (jd *HandleT) assert(cond bool) {
	if !cond {
		debug.SetTraceback("all")
		debug.PrintStack()
		jd.printLists(true)
		fmt.Println(jd.dsEmptyResultCache)
		panic("Assertion failed")
	}
}

var validJobStates = map[string]bool{
	"NP":            false, //False means internal state
	"succeeded":     true,
	"failed":        true,
	"executing":     true,
	"aborted":       true,
	"waiting":       true,
	"waiting_retry": true,
}

func (jd *HandleT) checkValidJobState(stateFilters []string) {
	for _, st := range stateFilters {
		_, ok := validJobStates[st]
		jd.assert(ok)
	}
}

//DB connection related parameters
const (
	host     = "localhost"
	port     = 5432
	user     = "ubuntu"
	password = "ubuntu"
	dbname   = "ubuntu"
)

/*Migration related parameters
jobDoneMigrateThres: A DS is migrated when this fraction of the jobs have been processed
jobStatusMigrateThres: A DS is migrated if the job_status exceeds this (* no_of_jobs)
maxDSSize: Maximum size of a DS. The process which adds new DS runs in the background
           (every few seconds) so a DS may go beyond this size
maxMigrateOnce: Maximum number of DSs that are migrated together into one destination
mainCheckSleepDuration: How often is the loop (which checks for adding/migrating DS) run
*/

const (
	jobDoneMigrateThres    = 0.8
	jobStatusMigrateThres  = 5
	maxDSSize              = 100000
	maxMigrateOnce         = 10
	mainCheckSleepDuration = (2 * time.Second)
)

/*
Setup is used to initialize the HandleT structure.
clearAll = True means it will remove all existing tables
tablePrefix must be unique and is used to separate
multiple users of JobsDB
dsRetentionPeriod = A DS is not deleted if it has some activity
in the retention time
*/
func (jd *HandleT) Setup(clearAll bool, tablePrefix string, retentionPeriod time.Duration) {

	var err error
	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s "+
		"password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)

	jd.assert(tablePrefix != "")
	jd.tablePrefix = tablePrefix
	jd.dsRetentionPeriod = retentionPeriod
	jd.dsEmptyResultCache = map[dataSetT]map[string]map[string]bool{}

	jd.dbHandle, err = sql.Open("postgres", psqlInfo)
	jd.assertError(err)

	log.Println("Connected to DB")
	err = jd.dbHandle.Ping()
	jd.assertError(err)

	log.Println("Sent Ping")

	jd.setupEnumTypes()

	if clearAll {
		jd.dropAllDS()
		jd.delJournal()
	} else {
		jd.recoverFromJournal()
	}

	//Refresh in memory list. We don't take lock
	//here because this is called before anything
	//else
	jd.getDSList(true)
	jd.getDSRangeList(true)

	//If no DS present, add one
	if len(jd.datasetList) == 0 {
		jd.createJournal()
		jd.addNewDS(true, dataSetT{})
	}

	go jd.mainCheckLoop()
}

/*
TearDown releases all the resources
*/
func (jd *HandleT) TearDown() {
	jd.dbHandle.Close()
}

/*
Function to return an ordered list of datasets and datasetRanges
Most callers use the in-memory list of dataset and datasetRanges
Caller must have the dsListLock readlocked
*/
func (jd *HandleT) getDSList(refreshFromDB bool) []dataSetT {

	if !refreshFromDB {
		return jd.datasetList
	}

	//At this point we MUST have write-locked dsListLock
	//since we are modiying the list

	//Reset the global list
	jd.datasetList = nil

	//Read the table names from PG
	stmt, err := jd.dbHandle.Prepare(`SELECT tablename
                                        FROM pg_catalog.pg_tables
                                        WHERE schemaname != 'pg_catalog' AND
                                        schemaname != 'information_schema'`)
	jd.assertError(err)
	defer stmt.Close()

	rows, err := stmt.Query()
	defer rows.Close()
	jd.assertError(err)

	tableNames := []string{}
	for rows.Next() {
		var tbName string
		err = rows.Scan(&tbName)
		jd.assertError(err)
		tableNames = append(tableNames, tbName)
	}

	//Tables are of form jobs_ and job_status_. Iterate
	//through them and sort them to produce and
	//ordered list of datasets

	jobNameMap := map[string]string{}
	jobStatusNameMap := map[string]string{}
	dnumList := []string{}

	for _, t := range tableNames {
		if strings.HasPrefix(t, jd.tablePrefix+"_jobs_") {
			dnum := t[len(jd.tablePrefix+"_jobs_"):]
			jobNameMap[dnum] = t
			dnumList = append(dnumList, dnum)
			continue
		}
		if strings.HasPrefix(t, jd.tablePrefix+"_job_status_") {
			dnum := t[len(jd.tablePrefix+"_job_status_"):]
			jobStatusNameMap[dnum] = t
			continue
		}
	}
	if len(dnumList) == 0 {
		return jd.datasetList
	}

	//Sort the suffixes. We should not have any use case
	//for having > 2 len suffixes (e.g. 1_1_1 - see comment below)
	//but this sort handles the general case

	sort.Slice(dnumList, func(i, j int) bool {
		src := strings.Split(dnumList[i], "_")
		dst := strings.Split(dnumList[j], "_")
		k := 0
		for {
			if k >= len(src) {
				//src has same prefix but is shorter
				//For example, src=1.1 while dest=1.1.1
				jd.assert(k < len(dst))
				jd.assert(k > 0)
				return true
			}
			if k >= len(dst) {
				//Opposite of case above
				jd.assert(k > 0)
				jd.assert(k < len(src))
				return false
			}
			if src[k] == dst[k] {
				//Loop
				k++
				continue
			}
			//Strictly ordered. Return
			srcInt, err := strconv.Atoi(src[k])
			jd.assertError(err)
			dstInt, err := strconv.Atoi(dst[k])
			jd.assertError(err)
			return srcInt < dstInt
		}
	})

	//Create the structure
	for _, dnum := range dnumList {
		jobName, ok := jobNameMap[dnum]
		jd.assert(ok)
		jobStatusName, ok := jobStatusNameMap[dnum]
		jd.assert(ok)
		jd.datasetList = append(jd.datasetList,
			dataSetT{JobTable: jobName,
				JobStatusTable: jobStatusName, Index: dnum})
	}
	return jd.datasetList
}

//Function must be called with read-lock held in dsListLock
func (jd *HandleT) getDSRangeList(refreshFromDB bool) []dataSetRangeT {

	var minID, maxID sql.NullInt64
	var prevMax int64

	if !refreshFromDB {
		return jd.datasetRangeList
	}

	//At this point we must have write-locked dsListLock
	dsList := jd.getDSList(true)
	jd.datasetRangeList = nil

	for idx, ds := range dsList {
		jd.assert(ds.Index != "")
		sqlStatement := fmt.Sprintf(`SELECT MIN(job_id), MAX(job_id) FROM %s`, ds.JobTable)
		row := jd.dbHandle.QueryRow(sqlStatement)
		err := row.Scan(&minID, &maxID)
		jd.assertError(err)
		log.Println(sqlStatement, minID, maxID)
		//We store ranges EXCEPT for the last element
		//which is being actively written to.
		if idx < len(dsList)-1 {
			jd.assert(minID.Valid && maxID.Valid)
			jd.assert(idx == 0 || prevMax < minID.Int64)
			jd.datasetRangeList = append(jd.datasetRangeList,
				dataSetRangeT{minJobID: int64(minID.Int64),
					maxJobID: int64(maxID.Int64), ds: ds})
			prevMax = maxID.Int64
		}
	}
	return jd.datasetRangeList
}

/*
Functions for checking when DB is full or DB needs to be migrated.
We migrate the DB ONCE most of the jobs have been processed (suceeded/aborted)
Or when the job_status table gets too big because of lot of retries/failures
*/

func (jd *HandleT) checkIfMigrateDS(ds dataSetT) (bool, int) {

	var delCount, totalCount, statusCount int

	sqlStatement := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, ds.JobTable)
	row := jd.dbHandle.QueryRow(sqlStatement)
	err := row.Scan(&totalCount)
	jd.assertError(err)

	//Jobs which have either succeded or expired
	sqlStatement = fmt.Sprintf(`SELECT COUNT(DISTINCT(id)) 
                                      FROM %s 
                                      WHERE job_state = 'succeeded' OR
                                            job_state = 'aborted'`, ds.JobStatusTable)
	row = jd.dbHandle.QueryRow(sqlStatement)
	err = row.Scan(&delCount)
	jd.assertError(err)

	//Total number of job status. If this table grows too big (e.g. lot of retries)
	//we migrate to a new table and get rid of old job status
	sqlStatement = fmt.Sprintf(`SELECT COUNT(*) FROM %s`, ds.JobStatusTable)
	row = jd.dbHandle.QueryRow(sqlStatement)
	err = row.Scan(&statusCount)
	jd.assertError(err)

	if totalCount == 0 {
		jd.assert(delCount == 0 && statusCount == 0)
		return false, 0
	}

	//If records are newer than what is required. One example use case is
	//gateway DB where records are kept to dedup

	var lastUpdate time.Time
	sqlStatement = fmt.Sprintf(`SELECT MAX(created_at) FROM %s`, ds.JobTable)
	row = jd.dbHandle.QueryRow(sqlStatement)
	err = row.Scan(&lastUpdate)
	jd.assertError(err)

	if jd.dsRetentionPeriod > time.Duration(0) && time.Since(lastUpdate) < jd.dsRetentionPeriod {
		return false, totalCount - delCount
	}

	if (float64(delCount)/float64(totalCount) > jobDoneMigrateThres) ||
		(float64(statusCount)/float64(totalCount) > jobStatusMigrateThres) {
		return true, totalCount - delCount
	}
	return false, totalCount - delCount
}

func (jd *HandleT) checkIfFullDS(ds dataSetT) bool {

	var totalCount int

	sqlStatement := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, ds.JobTable)
	row := jd.dbHandle.QueryRow(sqlStatement)
	err := row.Scan(&totalCount)
	jd.assertError(err)

	if totalCount > maxDSSize {
		return true
	}
	return false
}

/*
Function to add a new dataset. DataSet can be added to the end (e.g when last
becomes full OR in between during migration. DataSets are assigned numbers
monotonically when added  to end. So, with just add to end, numbers would be
like 1,2,3,4, and so on. Theese are called level0 datasets. And the Index is
called level0 Index
During migration, we add datasets in between. In the example above, if we migrate
1 & 2, we would need to create a new DS between 2 & 3. This is assigned the
the number 2_1. This is called a level1 dataset and the Index (2_1) is called level1
Index. We may migrate 2_1 into 2_2 and so on so there may be multiple level 1 datasets.

Immediately after creating a level_1 dataset (2_1 above), everything prior to it is
deleted. Hence there should NEVER be any requirement for having more than two levels.
*/

func (jd *HandleT) mapDSToLevel(ds dataSetT) (int, []int) {
	indexStr := strings.Split(ds.Index, "_")
	if len(indexStr) == 1 {
		indexLevel0, err := strconv.Atoi(indexStr[0])
		jd.assertError(err)
		return 1, []int{indexLevel0}
	}
	jd.assert(len(indexStr) == 2)
	indexLevel0, err := strconv.Atoi(indexStr[0])
	jd.assertError(err)
	indexLevel1, err := strconv.Atoi(indexStr[1])
	jd.assertError(err)
	return 2, []int{indexLevel0, indexLevel1}
}

func (jd *HandleT) createTableNames(dsIdx string) (string, string) {
	jobTable := fmt.Sprintf("%s_jobs_%s", jd.tablePrefix, dsIdx)
	jobStatusTable := fmt.Sprintf("%s_job_status_%s", jd.tablePrefix, dsIdx)
	return jobTable, jobStatusTable
}

func (jd *HandleT) addNewDS(appendLast bool, insertBeforeDS dataSetT) dataSetT {

	//Get the max index
	dList := jd.getDSList(true)
	newDSIdx := ""
	if appendLast {
		if len(dList) == 0 {
			newDSIdx = "1"
		} else {
			//Last one can only be Level0
			levels, levelVals := jd.mapDSToLevel(dList[len(dList)-1])
			jd.assert(levels == 1)
			newDSIdx = fmt.Sprintf("%d", levelVals[0]+1)
		}
	} else {
		jd.assert(len(dList) > 0)
		for idx, ds := range dList {
			if ds.Index == insertBeforeDS.Index {
				//We never insert before the first element
				jd.assert(idx > 0)
				levels, levelVals := jd.mapDSToLevel(ds)
				levelsPre, levelPreVals := jd.mapDSToLevel(dList[idx-1])
				//Some sanity checks (see comment above)
				//Insert before is never required on level2.
				//The level0 must be different by one
				jd.assert(levels == 1)
				jd.assert(levelVals[0] == levelPreVals[0]+1)
				if levelsPre == 1 {
					newDSIdx = fmt.Sprintf("%d_%d", levelPreVals[0], 1)
				} else {
					jd.assert(levelsPre == 2)
					newDSIdx = fmt.Sprintf("%d_%d", levelPreVals[0], levelPreVals[1]+1)
				}
			}

		}
	}
	jd.assert(newDSIdx != "")

	var newDS dataSetT
	newDS.JobTable, newDS.JobStatusTable = jd.createTableNames(newDSIdx)
	newDS.Index = newDSIdx

	//Mark the start of operation. If we crash somewhere here, we delete the
	//DS being added
	opPayload, err := json.Marshal(&journalOpPayloadT{To: newDS})
	jd.assertError(err)
	opID := jd.journalMarkStart(addDSOperation, opPayload)
	defer jd.journalMarkDone(opID)

	//Create the jobs and job_status tables
	sqlStatement := fmt.Sprintf(`CREATE TABLE %s (
                                      job_id BIGSERIAL PRIMARY KEY,
                                      uuid UUID NOT NULL,
                                      custom_val VARCHAR(64) NOT NULL,
                                      event_payload JSONB NOT NULL,
                                      created_at TIMESTAMP NOT NULL,
                                      expire_at TIMESTAMP NOT NULL);`, newDS.JobTable)

	_, err = jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	sqlStatement = fmt.Sprintf(`CREATE TABLE %s (
                                     id BIGSERIAL PRIMARY KEY,
                                     job_id INT REFERENCES %s(job_id),
                                     job_state job_state_type,
                                     attempt SMALLINT,
                                     exec_time TIMESTAMP,
                                     retry_time TIMESTAMP,
                                     error_code VARCHAR(32),
                                     error_response JSONB);`, newDS.JobStatusTable, newDS.JobTable)
	_, err = jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	if appendLast {
		//Refresh the in-memory list. We only need to refresh the
		//last DS, not the entire but we do it anyway.
		//For the range list, we use the cached data. Internally
		//it queries the new dataset which was added.
		dList = jd.getDSList(true)
		dRangeList := jd.getDSRangeList(true)

		//We should not have range values for the last element (the new DS)
		jd.assert(len(dList) == len(dRangeList)+1)

		//Now set the min JobID for the new DS just added to be 1 more than previous max
		if len(dRangeList) > 0 {
			newDSMin := dRangeList[len(dRangeList)-1].maxJobID
			jd.assert(newDSMin > 0)
			sqlStatement = fmt.Sprintf(`SELECT setval('%s_jobs_%s_job_id_seq', %d)`,
				jd.tablePrefix, newDSIdx, newDSMin)
			_, err = jd.dbHandle.Exec(sqlStatement)
			jd.assertError(err)
		}
		return dList[len(dList)-1]
	}
	//This is the migration case. We don't yet update the in-memory list till
	//we finish the migration
	return newDS
}

//Drop a dataset
func (jd *HandleT) dropDS(ds dataSetT, allowMissing bool) {

	//Doing if exists only if caller explicitly mentions
	//that its okay for DB to be missing. This scenario
	//happens during recovering from failed migration.
	//For every other case, the table must exist
	var sqlStatement string
	if allowMissing {
		sqlStatement = fmt.Sprintf(`DROP TABLE IF EXISTS %s`, ds.JobStatusTable)
	} else {
		sqlStatement = fmt.Sprintf(`DROP TABLE %s`, ds.JobStatusTable)
	}
	_, err := jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	if allowMissing {
		sqlStatement = fmt.Sprintf(`DROP TABLE IF EXISTS %s`, ds.JobTable)
	} else {
		sqlStatement = fmt.Sprintf(`DROP TABLE %s`, ds.JobTable)
	}

	_, err = jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)
}

func (jd *HandleT) dropAllDS() error {

	jd.dsListLock.Lock()
	defer jd.dsListLock.Unlock()

	dList := jd.getDSList(true)
	for _, ds := range dList {
		jd.dropDS(ds, false)
	}

	//Update the list
	jd.getDSList(true)
	jd.getDSRangeList(true)

	return nil
}

/*
Function to migrate jobs from src dataset  (srcDS) to destination dataset (dest_ds)
First all the unprocessed jobs are copied over. Then all the jobs which haven't
completed (state is failed or waiting or waiting_retry or executiong) are copied
over. Then the status (only the latest) is set for those jobs
*/

func (jd *HandleT) migrateJobs(srcDS dataSetT, destDS dataSetT) error {

	//Unprocessed jobs
	unprocessedList, err := jd.getUnprocessedJobsDS(srcDS, []string{}, false, 0)
	jd.assertError(err)

	//Jobs which haven't finished processing
	retryList, err := jd.getProcessedJobsDS(srcDS, true,
		[]string{"failed", "waiting", "waiting_retry", "executing"}, []string{}, 0)

	jd.assertError(err)

	//Copy the jobs over. Second parameter (true) makes sure job_id is copied over
	//instead of getting auto-assigned
	err = jd.storeJobsDS(destDS, true, append(unprocessedList, retryList...))
	jd.assertError(err)

	//Now copy over the latest status of the unfinished jobs
	var statusList []*JobStatusT
	for _, job := range retryList {
		newStatus := JobStatusT{
			JobID:         job.JobID,
			JobState:      job.LastJobStatus.JobState,
			AttemptNum:    job.LastJobStatus.AttemptNum,
			ExecTime:      job.LastJobStatus.ExecTime,
			RetryTime:     job.LastJobStatus.RetryTime,
			ErrorCode:     job.LastJobStatus.ErrorCode,
			ErrorResponse: job.LastJobStatus.ErrorResponse,
		}
		statusList = append(statusList, &newStatus)
	}
	jd.updateJobStatusDS(destDS, statusList, []string{})

	return nil
}

func (jd *HandleT) postMigrateDeleteDS(migrateFrom []dataSetT) error {

	//Now drop the source. Ideally we should dump into S3
	for _, ds := range migrateFrom {
		jd.dropDS(ds, false)
	}

	//Refresh the in-memory lists
	jd.getDSList(true)
	jd.getDSRangeList(true)

	return nil
}

/*
Next set of functions are for reading/writing jobs and job_status for
a given dataset. The names should be self explainatory
*/

func (jd *HandleT) storeJobsDS(ds dataSetT, copyID bool, jobList []*JobT) (ret error) {

	var stmt *sql.Stmt
	var err error

	//Using transactions for bulk copying
	txn, err := jd.dbHandle.Begin()
	jd.assertError(err)

	if copyID {
		stmt, err = txn.Prepare(pq.CopyIn(ds.JobTable, "job_id", "uuid", "custom_val",
			"event_payload", "created_at", "expire_at"))
		jd.assertError(err)
	} else {
		stmt, err = txn.Prepare(pq.CopyIn(ds.JobTable, "uuid", "custom_val", "event_payload",
			"created_at", "expire_at"))
		jd.assertError(err)
	}

	defer stmt.Close()
	for _, job := range jobList {
		if copyID {
			_, err = stmt.Exec(job.JobID, job.UUID, job.CustomVal,
				string(job.EventPayload), job.CreatedAt, job.ExpireAt)
		} else {
			_, err = stmt.Exec(job.UUID, job.CustomVal, string(job.EventPayload),
				job.CreatedAt, job.ExpireAt)
		}
		jd.assertError(err)
	}
	_, err = stmt.Exec()
	jd.assertError(err)

	err = txn.Commit()
	jd.assertError(err)

	//Empty customValFilters means we want to clear for all
	jd.markClearEmptyResult(ds, []string{}, []string{}, false)

	return nil
}

func (jd *HandleT) constructQuery(paramKey string, paramList []string, queryType string) string {
	jd.assert(queryType == "OR" || queryType == "AND")
	var queryList []string
	for _, p := range paramList {
		queryList = append(queryList, "("+paramKey+"='"+p+"')")
	}
	return "(" + strings.Join(queryList, " "+queryType+" ") + ")"
}

/*
* If a query returns empty result for a specific dataset, we cache that so that
* future queries don't have to hit the DB.
* markClearEmptyResult() when mark=True marks dataset,customVal,state as empty.
* markClearEmptyResult() when mark=False clears a previous empty mark
 */

func (jd *HandleT) markClearEmptyResult(ds dataSetT, stateFilters []string, customValFilters []string, mark bool) {

	jd.dsCacheLock.Lock()
	defer jd.dsCacheLock.Unlock()

	//This means we want to mark/clear all customVals and stateFilters
	//When clearing, we remove the entire dataset entry. Not a big issue
	//We process ALL only during internal migration and caching empty
	//results is not important
	if len(stateFilters) == 0 || len(customValFilters) == 0 {
		if mark == false {
			delete(jd.dsEmptyResultCache, ds)
		}
		return
	}

	_, ok := jd.dsEmptyResultCache[ds]
	if !ok {
		jd.dsEmptyResultCache[ds] = map[string]map[string]bool{}
	}

	for _, cVal := range customValFilters {
		_, ok := jd.dsEmptyResultCache[ds][cVal]
		if !ok {
			jd.dsEmptyResultCache[ds][cVal] = map[string]bool{}
		}
		for _, st := range stateFilters {
			if mark {
				jd.dsEmptyResultCache[ds][cVal][st] = true
			} else {
				jd.dsEmptyResultCache[ds][cVal][st] = false
			}
		}
	}
}

func (jd *HandleT) isEmptyResult(ds dataSetT, stateFilters []string, customValFilters []string) bool {

	jd.dsCacheLock.Lock()
	defer jd.dsCacheLock.Unlock()

	_, ok := jd.dsEmptyResultCache[ds]
	if !ok {
		return false
	}
	//We want to check for all states and customFilters. Cannot
	//assert that from cache
	if len(stateFilters) == 0 || len(customValFilters) == 0 {
		return false
	}

	for _, cVal := range customValFilters {
		_, ok := jd.dsEmptyResultCache[ds][cVal]
		if !ok {
			return false
		}
		for _, st := range stateFilters {
			mark, ok := jd.dsEmptyResultCache[ds][cVal][st]
			if !ok || mark == false {
				return false
			}
		}
	}
	//Every state and every customVal in the DS is empty
	//so can return
	return true
}

//limitCount == 0 means return all
func (jd *HandleT) getProcessedJobsDS(ds dataSetT, getAll bool, stateFilters []string,
	customValFilters []string, limitCount int) ([]*JobT, error) {

	var stateQuery, customValQuery, limitQuery string

	jd.checkValidJobState(stateFilters)

	if jd.isEmptyResult(ds, stateFilters, customValFilters) {
		return []*JobT{}, nil
	}

	if len(stateFilters) > 0 {
		stateQuery = " AND " + jd.constructQuery("job_state", stateFilters, "OR")
	} else {
		stateQuery = ""
	}
	if len(customValFilters) > 0 {
		jd.assert(!getAll)
		customValQuery = " AND " +
			jd.constructQuery(fmt.Sprintf("%s.custom_val", ds.JobTable),
				customValFilters, "OR")
	} else {
		customValQuery = ""
	}

	if limitCount > 0 {
		jd.assert(!getAll)
		limitQuery = fmt.Sprintf(" LIMIT %d ", limitCount)
	} else {
		limitQuery = ""
	}

	var rows *sql.Rows
	if getAll {
		sqlStatement := fmt.Sprintf(`SELECT 
                                  %[1]s.job_id, %[1]s.uuid, %[1]s.custom_val, %[1]s.event_payload,
                                  %[1]s.created_at, %[1]s.expire_at, 
                                  job_latest_state.job_state, job_latest_state.attempt,
                                  job_latest_state.exec_time, job_latest_state.retry_time,
                                  job_latest_state.error_code, job_latest_state.error_response
                                 FROM  
                                  %[1]s, 
                                  (SELECT job_id, job_state, attempt, exec_time, retry_time, 
                                    error_code, error_response FROM %[2]s WHERE id IN 
                                    (SELECT MAX(id) from %[2]s GROUP BY job_id) %[3]s)
                                  AS job_latest_state 
                                   WHERE %[1]s.job_id=job_latest_state.job_id`,
			ds.JobTable, ds.JobStatusTable, stateQuery)
		var err error
		rows, err = jd.dbHandle.Query(sqlStatement)
		defer rows.Close()
		jd.assertError(err)
	} else {
		sqlStatement := fmt.Sprintf(`SELECT 
                                               %[1]s.job_id, %[1]s.uuid, %[1]s.custom_val, %[1]s.event_payload,
                                               %[1]s.created_at, %[1]s.expire_at, 
                                               job_latest_state.job_state, job_latest_state.attempt,
                                               job_latest_state.exec_time, job_latest_state.retry_time,
                                               job_latest_state.error_code, job_latest_state.error_response
                                            FROM  
                                               %[1]s, 
                                               (SELECT job_id, job_state, attempt, exec_time, retry_time,
                                                 error_code, error_response FROM %[2]s WHERE id IN 
                                                   (SELECT MAX(id) from %[2]s GROUP BY job_id) %[3]s) 
                                               AS job_latest_state 
                                            WHERE %[1]s.job_id=job_latest_state.job_id 
                                             %[4]s
                                             AND job_latest_state.retry_time < $1 ORDER BY %[1]s.job_id %[5]s`,
			ds.JobTable, ds.JobStatusTable, stateQuery, customValQuery, limitQuery)
		//log.Println(sqlStatement)
		stmt, err := jd.dbHandle.Prepare(sqlStatement)

		jd.assertError(err)
		defer stmt.Close()
		rows, err = stmt.Query(time.Now())
		defer rows.Close()
		jd.assertError(err)
	}

	var jobList []*JobT
	for rows.Next() {
		var job JobT
		err := rows.Scan(&job.JobID, &job.UUID, &job.CustomVal,
			&job.EventPayload, &job.CreatedAt, &job.ExpireAt,
			&job.LastJobStatus.JobState, &job.LastJobStatus.AttemptNum,
			&job.LastJobStatus.ExecTime, &job.LastJobStatus.RetryTime,
			&job.LastJobStatus.ErrorCode, &job.LastJobStatus.ErrorResponse)
		jd.assertError(err)
		jobList = append(jobList, &job)
	}

	if len(jobList) == 0 {
		jd.markClearEmptyResult(ds, stateFilters, customValFilters, true)
	}

	return jobList, nil
}

//count == 0 means return all
func (jd *HandleT) getUnprocessedJobsDS(ds dataSetT, customValFilters []string,
	order bool, count int) ([]*JobT, error) {

	var rows *sql.Rows
	var err error

	if jd.isEmptyResult(ds, []string{"NP"}, customValFilters) {
		return []*JobT{}, nil
	}

	sqlStatement := fmt.Sprintf(`SELECT %[1]s.job_id, %[1]s.uuid, %[1]s.custom_val,
                                               %[1]s.event_payload, %[1]s.created_at, 
                                               %[1]s.expire_at 
                                             FROM %[1]s LEFT JOIN %[2]s ON %[1]s.job_id=%[2]s.job_id 
                                             WHERE %[2]s.job_id is NULL`, ds.JobTable, ds.JobStatusTable)

	//log.Println(sqlStatement)

	if len(customValFilters) > 0 {
		sqlStatement += " AND " + jd.constructQuery(fmt.Sprintf("%s.custom_val", ds.JobTable),
			customValFilters, "OR")
	}
	if order {
		sqlStatement += fmt.Sprintf(" ORDER BY %s.job_id", ds.JobTable)
	}
	if count > 0 {
		sqlStatement += fmt.Sprintf(" LIMIT %d", count)
	}

	rows, err = jd.dbHandle.Query(sqlStatement)
	defer rows.Close()
	jd.assertError(err)

	var jobList []*JobT
	for rows.Next() {
		var job JobT
		err := rows.Scan(&job.JobID, &job.UUID, &job.CustomVal,
			&job.EventPayload, &job.CreatedAt, &job.ExpireAt)
		jd.assertError(err)
		jobList = append(jobList, &job)
	}

	if len(jobList) == 0 {
		jd.markClearEmptyResult(ds, []string{"NP"}, customValFilters, true)
	}

	return jobList, nil
}

func (jd *HandleT) updateJobStatusDS(ds dataSetT, statusList []*JobStatusT, customValFilters []string) (err error) {

	if len(statusList) == 0 {
		return nil
	}

	txn, err := jd.dbHandle.Begin()
	jd.assertError(err)

	stmt, err := txn.Prepare(pq.CopyIn(ds.JobStatusTable, "job_id", "job_state", "attempt", "exec_time",
		"retry_time", "error_code", "error_response"))
	jd.assertError(err)

	defer stmt.Close()
	for _, status := range statusList {
		_, err = stmt.Exec(status.JobID, status.JobState, status.AttemptNum, status.ExecTime,
			status.RetryTime, status.ErrorCode, string(status.ErrorResponse))
		jd.assertError(err)
	}
	_, err = stmt.Exec()
	jd.assertError(err)

	err = txn.Commit()
	jd.assertError(err)

	//Get all the states and clear from empty cache
	stateFiltersMap := map[string]bool{}
	for _, st := range statusList {
		stateFiltersMap[st.JobState] = true
	}
	stateFilters := make([]string, 0, len(stateFiltersMap))
	for k := range stateFiltersMap {
		stateFilters = append(stateFilters, k)
	}

	jd.markClearEmptyResult(ds, stateFilters, customValFilters, false)

	return nil
}

/**
The next set of functions are the user visible functions to get/set job status.
For reading jobs, it scans from the oldest DS to the latest till it has found
enough jobs. For updating status, it finds the DS to which the job belongs
(using the in-memory range list) and adds the status to the appropriate DS.
These functions can race with the internal function to add new DS and create
new DS. Synchronization is handled by locks as described below.

In theory, we can keep just one lock. All operations which
change the DS structure (e.g. adding new dataset or moving records
from one DS to another thearby updating the DS range) can take a write lock
while functions which don't update the DS structure (as in list of DS or
ranges within DS can take the read lock) as they can run in paralle.

The drawback with this approach is that migrating a DS can take a long
time and can potentially block the StoreJob() call. Blocking StoreJob()
is bad since user ACK won't be sent unless StoreJob() returns.

To handle this, we separate out the locks into dsListLock and dsMigrationLock.
Store() only needs to access the last element of dsList and is not
impacted by movement of data across ds so it only takes the dsListLock.
Other functions are impacted by movement of data across DS in background
so take both the list and data lock

*/

func (jd *HandleT) mainCheckLoop() {

	for {
		time.Sleep(mainCheckSleepDuration)
		log.Println("Main check:Start")
		jd.dsListLock.RLock()
		dsList := jd.getDSList(false)
		jd.dsListLock.RUnlock()
		latestDS := dsList[len(dsList)-1]
		if jd.checkIfFullDS(latestDS) {
			//Adding a new DS updates the list
			//Doesn't move any data so we only
			//take the list lock
			jd.dsListLock.Lock()
			log.Println("Main check:NewDS")
			jd.addNewDS(true, dataSetT{})
			jd.dsListLock.Unlock()
		}

		//Take the lock and run actual migration
		jd.dsMigrationLock.Lock()

		var migrateFrom []dataSetT
		var insertBeforeDS dataSetT
		var liveCount int
		for idx, ds := range dsList {
			ifMigrate, remCount := jd.checkIfMigrateDS(ds)
			log.Println("Migrate check", ifMigrate, ds)
			if idx < len(dsList)-1 && ifMigrate && idx < maxMigrateOnce && liveCount < maxDSSize {
				migrateFrom = append(migrateFrom, ds)
				insertBeforeDS = dsList[idx+1]
				liveCount += remCount
			} else {
				//We migrate from the leftmost onwards
				//If we cannot migrate one, we stop
				break
			}
		}
		//Add a temp DS to append to
		if len(migrateFrom) > 0 {
			if liveCount > 0 {
				jd.dsListLock.Lock()
				migrateTo := jd.addNewDS(false, insertBeforeDS)
				jd.dsListLock.Unlock()

				log.Println("Migrate from:", migrateFrom)
				log.Println("Next:", insertBeforeDS)
				log.Println("To:", migrateTo)
				//Mark the start of copy operation. If we fail here
				//we just delete the new DS being copied into. The
				//sources are still around
				opPayload, err := json.Marshal(&journalOpPayloadT{From: migrateFrom, To: migrateTo})
				jd.assertError(err)
				opID := jd.journalMarkStart(migrateCopyOperation, opPayload)

				for _, ds := range migrateFrom {
					log.Println("Main check:Migrate", ds, migrateTo)
					jd.migrateJobs(ds, migrateTo)
				}
				jd.journalMarkDone(opID)
			}

			//Mark the start of del operation. If we fail in between
			//we need to finish deleting the source datasets. Cannot
			//del the destination as some sources may have been deleted
			opPayload, err := json.Marshal(&journalOpPayloadT{From: migrateFrom})
			jd.assertError(err)
			opID := jd.journalMarkStart(migrateDelOperation, opPayload)

			jd.dsListLock.Lock()
			jd.postMigrateDeleteDS(migrateFrom)
			jd.dsListLock.Unlock()

			jd.journalMarkDone(opID)
		}

		jd.dsMigrationLock.Unlock()

	}
}

/*
We keep a journal of all the operations. The journal helps
*/
const (
	addDSOperation       = "ADD_DS"
	migrateCopyOperation = "MIGRATE_COPY"
	migrateDelOperation  = "MIGRATE_DEL"
)

func (jd *HandleT) createJournal() {

	sqlStatement := fmt.Sprintf(`CREATE TABLE %s_journal (
                                      id BIGSERIAL PRIMARY KEY,
                                      operation VARCHAR(32) NOT NULL,
                                      done BOOLEAN,
                                      operation_payload JSONB NOT NULL,
                                      start_time TIMESTAMP NOT NULL,
                                      end_time TIMESTAMP);`, jd.tablePrefix)

	_, err := jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)
}

func (jd *HandleT) delJournal() {

	sqlStatement := fmt.Sprintf(`DROP TABLE IF EXISTS %s_journal`, jd.tablePrefix)
	_, err := jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)
}

func (jd *HandleT) journalMarkStart(opType string, opPayload json.RawMessage) int64 {

	jd.assert(opType == addDSOperation || opType == migrateCopyOperation || opType == migrateDelOperation)

	sqlStatement := fmt.Sprintf(`INSERT INTO %s_journal (operation, done, operation_payload, start_time)  
                                       VALUES ($1, $2, $3, $4) RETURNING id`, jd.tablePrefix)
	stmt, err := jd.dbHandle.Prepare(sqlStatement)
	defer stmt.Close()

	var opID int64
	err = stmt.QueryRow(opType, false, opPayload, time.Now()).Scan(&opID)
	jd.assertError(err)

	return opID

}

func (jd *HandleT) journalMarkDone(opID int64) {
	sqlStatement := fmt.Sprintf(`UPDATE %s_journal SET done=$2, end_time=$3 WHERE id=$1`, jd.tablePrefix)
	_, err := jd.dbHandle.Exec(sqlStatement, opID, true, time.Now())
	jd.assertError(err)
}

func (jd *HandleT) recoverFromJournal() {

	sqlStatement := fmt.Sprintf(`SELECT id, operation, done, operation_payload 
                                     FROM %s_journal
                                     WHERE 
                                     done=False 
                                     ORDER BY id`, jd.tablePrefix)

	stmt, err := jd.dbHandle.Prepare(sqlStatement)
	defer stmt.Close()
	jd.assertError(err)

	rows, err := stmt.Query()
	jd.assertError(err)
	defer rows.Close()

	count := 0
	var opID int64
	var opType string
	var opDone bool
	var opPayload json.RawMessage
	var opPayloadJSON journalOpPayloadT
	var undoOp = false

	for rows.Next() {
		err = rows.Scan(&opID, &opType, &opDone, &opPayload)
		jd.assertError(err)
		jd.assert(opDone == false)
		count++
	}
	jd.assert(count <= 1)

	if count == 0 {
		//Nothing to recoer
		return
	}

	//Need to recover the last failed operation
	//Get the payload and undo
	err = json.Unmarshal(opPayload, &opPayloadJSON)
	jd.assertError(err)

	switch opType {
	case addDSOperation:
		newDS := opPayloadJSON.To
		undoOp = true
		//Drop the table we were tring to create
		fmt.Println("Recovering new DS operation", newDS)
		log.Println("Recovering new DS operation", newDS)
		jd.dropDS(newDS, true)
	case migrateCopyOperation:
		migrateDest := opPayloadJSON.To
		//Delete the destination of the interrupted
		//migration. After we start, code should
		//redo the migration
		fmt.Println("Recovering migrateCopy operation", migrateDest)
		log.Println("Recovering migrateCopy operation", migrateDest)
		jd.dropDS(migrateDest, true)
		undoOp = true
	case migrateDelOperation:
		//Some of the source datasets would have been
		migrateSrc := opPayloadJSON.From
		for _, ds := range migrateSrc {
			jd.dropDS(ds, true)
		}
		fmt.Println("Recovering migrateDel operation", migrateSrc)
		log.Println("Recovering migrateDel operation", migrateSrc)
		undoOp = false
	}

	if undoOp {
		sqlStatement = fmt.Sprintf(`DELETE FROM %s_journal WHERE id=$1`, jd.tablePrefix)
	} else {
		sqlStatement = fmt.Sprintf(`UPDATE %s_journal SET done=True WHERE id=$1`, jd.tablePrefix)
	}

	_, err = jd.dbHandle.Exec(sqlStatement, opID)
	jd.assertError(err)

}

/*
UpdateJobStatus updates the status of a batch of jobs
customValFilters[] is passed so we can efficinetly mark empty cache
Later we can move this to query
*/
func (jd *HandleT) UpdateJobStatus(statusList []*JobStatusT, customValFilters []string) {

	//First we sort by JobID
	sort.Slice(statusList, func(i, j int) bool {
		return statusList[i].JobID < statusList[j].JobID
	})

	//The order of lock is very important. The mainCheckLoop
	//takes lock in this order so reversing this will cause
	//deadlocks
	jd.dsMigrationLock.RLock()
	jd.dsListLock.RLock()
	defer jd.dsMigrationLock.RUnlock()
	defer jd.dsListLock.RUnlock()

	//We scan through the list of jobs and map them to DS
	var lastPos int
	dsRangeList := jd.getDSRangeList(false)
	for _, ds := range dsRangeList {
		minID := ds.minJobID
		maxID := ds.maxJobID
		//We have processed upto (but excluding) lastPos on statusList.
		//Hence that element must lie in this or subsequent dataset's
		//range
		jd.assert(statusList[lastPos].JobID >= minID)
		var i int
		for i = lastPos; i < len(statusList); i++ {
			//The JobID is outside this DS's range
			if statusList[i].JobID > maxID {
				if i > lastPos {
					log.Println("Range:", ds, statusList[lastPos].JobID,
						statusList[i-1].JobID, lastPos, i-1)
				}
				err := jd.updateJobStatusDS(ds.ds, statusList[lastPos:i], customValFilters)
				jd.assertError(err)
				lastPos = i
				break
			}
		}
		//Reached the end. Need to process this range
		if i == len(statusList) && lastPos < i {
			log.Println("Range:", ds, statusList[lastPos].JobID, statusList[i-1].JobID, lastPos, i)
			err := jd.updateJobStatusDS(ds.ds, statusList[lastPos:i], customValFilters)
			jd.assertError(err)
			lastPos = i
			break
		}
	}

	//The last (most active DS) might not have range element as it is being written to
	if lastPos < len(statusList) {
		//Make sure the last range is missing
		dsList := jd.getDSList(false)
		jd.assert(len(dsRangeList) == len(dsList)-1)
		//Update status in the last element
		log.Println("RangeEnd", statusList[lastPos].JobID, lastPos, len(statusList))
		err := jd.updateJobStatusDS(dsList[len(dsList)-1], statusList[lastPos:], customValFilters)
		jd.assertError(err)
	}

}

/*
Store call is used to create new Jobs
*/
func (jd *HandleT) Store(jobList []*JobT) {

	//Only locks the list
	jd.dsListLock.RLock()
	defer jd.dsListLock.RUnlock()

	dsList := jd.getDSList(false)
	jd.storeJobsDS(dsList[len(dsList)-1], false, jobList)

}

/*
printLists is a debuggging function used to print
the current in-memory copy of jobs and job ranges
*/
func (jd *HandleT) printLists(console bool) {

	//This being an internal function, we don't lock
	log.Println("List:", jd.getDSList(false))
	log.Println("Ranges:", jd.getDSRangeList(false))
	if console {
		fmt.Println("List:", jd.getDSList(false))
		fmt.Println("Ranges:", jd.getDSRangeList(false))
	}

}

/*
GetUnprocessed returns the unprocessed events. Unprocessed events are
those whose state hasn't been marked in the DB
*/
func (jd *HandleT) GetUnprocessed(customValFilters []string, count int) []*JobT {

	//The order of lock is very important. The mainCheckLoop
	//takes lock in this order so reversing this will cause
	//deadlocks
	jd.dsMigrationLock.RLock()
	jd.dsListLock.RLock()
	defer jd.dsMigrationLock.RUnlock()
	defer jd.dsListLock.RUnlock()

	dsList := jd.getDSList(false)
	outJobs := make([]*JobT, 0)
	jd.assert(count >= 0)
	if count == 0 {
		return outJobs
	}
	for _, ds := range dsList {
		//count==0 means return all which we don't want
		jd.assert(count > 0)
		jobs, err := jd.getUnprocessedJobsDS(ds, customValFilters, true, count)
		jd.assertError(err)
		outJobs = append(outJobs, jobs...)
		count -= len(jobs)
		jd.assert(count >= 0)
		if count == 0 {
			break
		}
	}

	//Release lock
	return outJobs
}

/*
GetProcessed returns events of a given state. This does not update any state itself and
relises on the caller to update it. That means that successive calls to GetProcessed("failed")
can return the same set of events. It is the responsibility of the caller to call it from
one thread, update the state (to "waiting") in the same thread and pass on the the processors
*/
func (jd *HandleT) GetProcessed(stateFilter []string, customValFilters []string, count int) []*JobT {

	//The order of lock is very important. The mainCheckLoop
	//takes lock in this order so reversing this will cause
	//deadlocks
	jd.dsMigrationLock.RLock()
	jd.dsListLock.RLock()
	defer jd.dsMigrationLock.RUnlock()
	defer jd.dsListLock.RUnlock()

	dsList := jd.getDSList(false)
	outJobs := make([]*JobT, 0)

	jd.assert(count >= 0)
	if count == 0 {
		return outJobs
	}

	for _, ds := range dsList {
		//count==0 means return all which we don't want
		jd.assert(count > 0)
		jobs, err := jd.getProcessedJobsDS(ds, false, stateFilter, customValFilters, count)
		jd.assertError(err)
		outJobs = append(outJobs, jobs...)
		count -= len(jobs)
		jd.assert(count >= 0)
		if count == 0 {
			break
		}
	}

	return outJobs
}

/*
GetToRetry returns events which need to be retried.
This is a wrapper over GetProcessed call above
*/
func (jd *HandleT) GetToRetry(customValFilters []string, count int) []*JobT {
	return jd.GetProcessed([]string{"failed"}, customValFilters, count)
}

/*
GetWaiting returns events which are under processing
This is a wrapper over GetProcessed call above
*/
func (jd *HandleT) GetWaiting(customValFilters []string, count int) []*JobT {
	return jd.GetProcessed([]string{"waiting"}, customValFilters, count)
}

/*
GetExecuting returns events which  in executing state
*/
func (jd *HandleT) GetExecuting(customValFilters []string, count int) []*JobT {
	return jd.GetProcessed([]string{"executing"}, customValFilters, count)
}

/*
================================================
==============Test Functions Below==============
================================================
*/

func (jd *HandleT) dropTables() error {
	sqlStatement := `DROP TABLE IF EXISTS job_status`
	_, err := jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	sqlStatement = `DROP TABLE IF EXISTS  jobs`
	_, err = jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	return nil

}

func (jd *HandleT) setupEnumTypes() {
	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s "+
		"password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)
	
	dbHandle, err := sql.Open("postgres", psqlInfo)
	defer dbHandle.Close()
	jd.assertError(err)	


	fmt.Println("Creating enum types in db")
	sqlStatement := `DO $$ BEGIN
                                CREATE TYPE job_state_type
                                     AS ENUM(
                                              'waiting',
                                              'executing',
                                              'succeeded',
                                              'waiting_retry',
                                              'failed',
                                              'aborted');
                                     EXCEPTION
                                        WHEN duplicate_object THEN null;
                            END $$;`

	_, err = dbHandle.Exec(sqlStatement)
	jd.assertError(err)
}

func (jd *HandleT) createTables() error {
	sqlStatement := `CREATE TABLE jobs (
                             job_id BIGSERIAL PRIMARY KEY,
                             uuid UUID NOT NULL,
                             custom_val INT NOT NULL,
                             event_payload JSONB NOT NULL,
                             created_at TIMESTAMP NOT NULL,
                             expire_at TIMESTAMP NOT NULL);`
	_, err := jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	sqlStatement = `CREATE TABLE job_status (
                            id BIGSERIAL PRIMARY KEY,
                            job_id INT REFERENCES jobs(job_id),
                            job_state job_state_type,
                            attempt SMALLINT,
                            exec_time TIMESTAMP,
                            retry_time TIMESTAMP,
                            error_code VARCHAR(32),
                            error_response JSONB);`
	_, err = jd.dbHandle.Exec(sqlStatement)
	jd.assertError(err)

	return nil
}

func (jd *HandleT) staticDSTest() {

	testEndPoint := "4"
	testNumRecs := 100001
	testNumQuery := 10000
	testFailRatio := 100

	jd.dropTables()
	jd.createTables()

	var jobList []*JobT
	for i := 0; i < testNumRecs; i++ {
		id := uuid.NewV4()
		newJob := JobT{
			UUID:         id,
			CreatedAt:    time.Now(),
			ExpireAt:     time.Now(),
			CustomVal:    testEndPoint,
			EventPayload: []byte(`{"event_type":"click"}`),
		}
		jobList = append(jobList, &newJob)
	}

	testDS := dataSetT{JobTable: "jobs", JobStatusTable: "job_status"}

	start := time.Now()
	jd.storeJobsDS(testDS, false, jobList)
	elapsed := time.Since(start)
	fmt.Println("Save", elapsed)

	for {
		start = time.Now()
		fmt.Println("Full:", jd.checkIfFullDS(testDS))
		elapsed = time.Since(start)
		fmt.Println("Checking DS", elapsed)

		start = time.Now()
		unprocessedList, _ := jd.getUnprocessedJobsDS(testDS, []string{testEndPoint}, true, testNumQuery)
		fmt.Println("Got unprocessed events:", len(unprocessedList))

		retryList, _ := jd.getProcessedJobsDS(testDS, false, []string{"failed"},
			[]string{testEndPoint}, testNumQuery)
		fmt.Println("Got retry events:", len(retryList))
		if len(unprocessedList)+len(retryList) == 0 {
			break
		}
		elapsed = time.Since(start)
		fmt.Println("Getting jobs", elapsed)

		//Mark call as executing
		var statusList []*JobStatusT
		for _, job := range append(unprocessedList, retryList...) {
			newStatus := JobStatusT{
				JobID:         job.JobID,
				JobState:      "executing",
				AttemptNum:    job.LastJobStatus.AttemptNum,
				ExecTime:      time.Now(),
				RetryTime:     time.Now(),
				ErrorCode:     "202",
				ErrorResponse: []byte(`{"success":"OK"}`),
			}
			statusList = append(statusList, &newStatus)
		}

		jd.updateJobStatusDS(testDS, statusList, []string{testEndPoint})

		//Mark call as failed
		statusList = nil
		var maxAttempt = 0
		for _, job := range append(unprocessedList, retryList...) {
			stat := "succeeded"
			if rand.Intn(testFailRatio) == 0 {
				stat = "failed"
			}
			if job.LastJobStatus.AttemptNum > maxAttempt {
				maxAttempt = job.LastJobStatus.AttemptNum
			}
			newStatus := JobStatusT{
				JobID:         job.JobID,
				JobState:      stat,
				AttemptNum:    job.LastJobStatus.AttemptNum + 1,
				ExecTime:      time.Now(),
				RetryTime:     time.Now(),
				ErrorCode:     "202",
				ErrorResponse: []byte(`{"success":"OK"}`),
			}
			statusList = append(statusList, &newStatus)
		}
		fmt.Println("Max attempt", maxAttempt)
		jd.updateJobStatusDS(testDS, statusList, []string{testEndPoint})
	}

}

func (jd *HandleT) dynamicDSTestMigrate() {

	testNumRecs := 10000
	testRuns := 5
	testEndPoint := "4"

	for i := 0; i < testRuns; i++ {
		jd.addNewDS(true, dataSetT{})
		var jobList []*JobT
		for i := 0; i < testNumRecs; i++ {
			id := uuid.NewV4()
			newJob := JobT{
				UUID:         id,
				CreatedAt:    time.Now(),
				ExpireAt:     time.Now(),
				CustomVal:    testEndPoint,
				EventPayload: []byte(`{"event_type":"click"}`),
			}
			jobList = append(jobList, &newJob)
		}
		jd.Store(jobList)
		fmt.Println(jd.getDSList(false))
		fmt.Println(jd.getDSRangeList(false))
		dsList := jd.getDSList(false)
		if i > 0 {
			jd.migrateJobs(dsList[0], dsList[1])
			jd.postMigrateDeleteDS([]dataSetT{dsList[0]})
		}
	}

}

func (jd *HandleT) dynamicTest() {

	testNumRecs := 10000
	testRuns := 20
	testEndPoint := "4"
	testNumQuery := 10000
	testFailRatio := 5

	for i := 0; i < testRuns; i++ {
		fmt.Println("Main process running")
		time.Sleep(1 * time.Second)
		var jobList []*JobT
		for i := 0; i < testNumRecs; i++ {
			id := uuid.NewV4()
			newJob := JobT{
				UUID:         id,
				CreatedAt:    time.Now(),
				ExpireAt:     time.Now(),
				CustomVal:    testEndPoint,
				EventPayload: []byte(`{"event_type":"click"}`),
			}
			jobList = append(jobList, &newJob)
		}
		jd.Store(jobList)
		jd.printLists(false)
	}

	for {

		time.Sleep(1 * time.Second)
		jd.printLists(false)

		start := time.Now()
		unprocessedList := jd.GetUnprocessed([]string{testEndPoint}, testNumQuery)
		fmt.Println("Got unprocessed events:", len(unprocessedList))

		retryList := jd.GetToRetry([]string{testEndPoint}, testNumQuery)
		fmt.Println("Got retry events:", len(retryList))
		if len(unprocessedList)+len(retryList) == 0 {
			break
		}
		elapsed := time.Since(start)
		fmt.Println("Getting jobs", elapsed)

		//Mark call as failed
		var statusList []*JobStatusT

		combinedList := append(unprocessedList, retryList...)
		sort.Slice(combinedList, func(i, j int) bool {
			return combinedList[i].JobID < combinedList[j].JobID
		})
		fmt.Println("Total:", len(combinedList), combinedList[0].JobID,
			combinedList[len(combinedList)-1].JobID)

		for _, job := range append(unprocessedList, retryList...) {
			stat := "succeeded"
			if rand.Intn(testFailRatio) == 0 {
				stat = "failed"
			}
			newStatus := JobStatusT{
				JobID:         job.JobID,
				JobState:      stat,
				AttemptNum:    job.LastJobStatus.AttemptNum + 1,
				ExecTime:      time.Now(),
				RetryTime:     time.Now(),
				ErrorCode:     "202",
				ErrorResponse: []byte(`{"success":"OK"}`),
			}
			statusList = append(statusList, &newStatus)
		}
		jd.UpdateJobStatus(statusList, []string{testEndPoint})
	}
}

/*
RunTest runs some internal tests
*/
func (jd *HandleT) RunTest() {
	jd.dynamicTest()
}