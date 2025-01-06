package korm

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kamalshkeir/aes"
	"github.com/kamalshkeir/argon"
	"github.com/kamalshkeir/kmap"
	"github.com/kamalshkeir/ksmux"
	"github.com/kamalshkeir/lg"
)

var termsessions = kmap.New[string, string]()

var LogsView = func(c *ksmux.Context) {
	d := map[string]any{
		"metrics": GetSystemMetrics(),
	}
	parsed := make([]LogEntry, 0)
	if v := lg.GetLogs(); v != nil {
		for _, vv := range reverseSlice(v.Slice) {
			parsed = append(parsed, parseLogString(vv))
		}
	}
	d["parsed"] = parsed
	c.Html("admin/admin_logs.html", d)
}

var DashView = func(c *ksmux.Context) {
	ddd := map[string]any{
		"withRequestCounter": withRequestCounter,
		"stats":              GetStats(),
	}
	if withRequestCounter {
		ddd["requests"] = GetTotalRequests()
	}

	c.Html("admin/admin_index.html", ddd)
}

var RestartView = func(c *ksmux.Context) {
	if serverBus != nil {
		lg.CheckError(serverBus.App.Restart())
	}
}

var TablesView = func(c *ksmux.Context) {
	allTables := GetAllTables(defaultDB)
	q := []string{}
	for _, t := range allTables {
		q = append(q, "SELECT '"+t+"' AS table_name,COUNT(*) AS count FROM "+t)
	}
	query := strings.Join(q, ` UNION ALL `)

	var results []struct {
		TableName string `db:"table_name"`
		Count     int    `db:"count"`
	}
	if err := To(&results).Query(query); lg.CheckError(err) {
		c.Error("something wrong happened")
		return
	}

	c.Html("admin/admin_tables.html", map[string]any{
		"tables":  allTables,
		"results": results,
	})
}

var LoginView = func(c *ksmux.Context) {
	c.Html("admin/admin_login.html", nil)
}

var LoginPOSTView = func(c *ksmux.Context) {
	requestData := c.BodyJson()
	email := requestData["email"]
	passRequest := requestData["password"]

	data, err := Table("users").Database(defaultDB).Where("email = ?", email).One()
	if err != nil {
		c.Status(http.StatusUnauthorized).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}
	if data["email"] == "" || data["email"] == nil {
		c.Status(http.StatusNotFound).Json(map[string]any{
			"error": "User doesn not Exist",
		})
		return
	}
	if data["is_admin"] == int64(0) || data["is_admin"] == 0 || data["is_admin"] == false {
		c.Status(http.StatusForbidden).Json(map[string]any{
			"error": "Not Allowed to access this page",
		})
		return
	}

	if passDB, ok := data["password"].(string); ok {
		if pp, ok := passRequest.(string); ok {
			if !argon.Match(passDB, pp) {
				c.Status(http.StatusForbidden).Json(map[string]any{
					"error": "Wrong Password",
				})
				return
			} else {
				if uuid, ok := data["uuid"].(string); ok {
					uuid, err = aes.Encrypt(uuid)
					lg.CheckError(err)
					c.SetCookie("session", uuid)
					c.Json(map[string]any{
						"success": "U Are Logged In",
					})
					return
				}
			}
		}
	}
}

var LogoutView = func(c *ksmux.Context) {
	c.DeleteCookie("session")
	c.Status(http.StatusTemporaryRedirect).Redirect("/")
}

var TableGetAll = func(c *ksmux.Context) {
	model := c.Param("model")
	if model == "" {
		c.Json(map[string]any{
			"error": "Error: No model given in params",
		})
		return
	}
	dbMem, _ := GetMemoryDatabase(defaultDB)
	if dbMem == nil {
		lg.ErrorC("unable to find db in mem", "db", defaultDB)
		dbMem = &databases[0]
	}
	idString := "id"
	var t *TableEntity
	for i, tt := range dbMem.Tables {
		if tt.Name == model {
			idString = tt.Pk
			t = &dbMem.Tables[i]
		}
	}

	var body struct {
		Page int `json:"page"`
	}
	if err := c.BodyStruct(&body); lg.CheckError(err) {
		c.Error("something wrong happened")
		return
	}
	if body.Page == 0 {
		body.Page = 1
	}
	rows, err := Table(model).Database(defaultDB).OrderBy("-" + idString).Limit(paginationPer).Page(body.Page).All()
	if err != nil {
		if err != ErrNoData {
			c.Status(404).Error("Unable to find this model")
			return
		}
		rows = []map[string]any{}
	}

	// Get total count for pagination
	var total int64
	var totalRows []int64
	err = To(&totalRows).Query("SELECT COUNT(*) FROM " + model)
	if err == nil {
		total = totalRows[0]
	}

	dbCols, cols := GetAllColumnsTypes(model)
	mmfkeysModels := map[string][]map[string]any{}
	mmfkeys := map[string][]any{}
	if t != nil {
		for _, fkey := range t.Fkeys {
			spFrom := strings.Split(fkey.FromTableField, ".")
			if len(spFrom) == 2 {
				spTo := strings.Split(fkey.ToTableField, ".")
				if len(spTo) == 2 {
					q := "select * from " + spTo[0] + " order by " + spTo[1]
					mm := []map[string]any{}
					err := To(&mm).Query(q)
					if !lg.CheckError(err) {
						ress := []any{}
						for _, res := range mm {
							ress = append(ress, res[spTo[1]])
						}
						if len(ress) > 0 {
							mmfkeys[spFrom[1]] = ress
							mmfkeysModels[spFrom[1]] = mm
							for _, v := range mmfkeysModels[spFrom[1]] {
								for i, vv := range v {
									if vvStr, ok := vv.(string); ok {
										if len(vvStr) > 20 {
											v[i] = vvStr[:20] + "..."
										}
									}
								}
							}
						}
					} else {
						lg.ErrorC("error:", "q", q, "spTo", spTo)
					}
				}
			}
		}
	} else {
		idString = cols[0]
	}

	if dbMem != nil {
		ccc := cols
		if t != nil {
			ccc = t.Columns
		}

		data := map[string]any{
			"dbType":         dbMem.Dialect,
			"table":          model,
			"rows":           rows,
			"total":          total,
			"dbcolumns":      dbCols,
			"pk":             idString,
			"fkeys":          mmfkeys,
			"fkeysModels":    mmfkeysModels,
			"columnsOrdered": ccc,
		}
		if t != nil {
			data["columns"] = t.ModelTypes
		} else {
			data["columns"] = dbCols
		}
		c.Json(map[string]any{
			"success": data,
		})
	} else {
		lg.ErrorC("table not found", "table", model)
		c.Status(404).Json(map[string]any{
			"error": "table not found",
		})
	}
}

var AllModelsGet = func(c *ksmux.Context) {
	model := c.Param("model")
	if model == "" {
		c.Json(map[string]any{
			"error": "Error: No model given in params",
		})
		return
	}

	dbMem, _ := GetMemoryDatabase(defaultDB)
	if dbMem == nil {
		lg.ErrorC("unable to find db in mem", "db", defaultDB)
		dbMem = &databases[0]
	}
	idString := "id"
	var t *TableEntity
	for i, tt := range dbMem.Tables {
		if tt.Name == model {
			idString = tt.Pk
			t = &dbMem.Tables[i]
		}
	}

	rows, err := Table(model).Database(defaultDB).OrderBy("-" + idString).Limit(paginationPer).Page(1).All()
	if err != nil {
		rows, err = Table(model).Database(defaultDB).All()
		if err != nil {
			if err != ErrNoData {
				c.Status(404).Error("Unable to find this model")
				return
			}
		}
	}
	dbCols, cols := GetAllColumnsTypes(model)
	mmfkeysModels := map[string][]map[string]any{}
	mmfkeys := map[string][]any{}
	if t != nil {
		for _, fkey := range t.Fkeys {
			spFrom := strings.Split(fkey.FromTableField, ".")
			if len(spFrom) == 2 {
				spTo := strings.Split(fkey.ToTableField, ".")
				if len(spTo) == 2 {
					q := "select * from " + spTo[0] + " order by " + spTo[1]
					mm := []map[string]any{}
					err := To(&mm).Query(q)
					if !lg.CheckError(err) {
						ress := []any{}
						for _, res := range mm {
							ress = append(ress, res[spTo[1]])
						}
						if len(ress) > 0 {
							mmfkeys[spFrom[1]] = ress
							mmfkeysModels[spFrom[1]] = mm
							for _, v := range mmfkeysModels[spFrom[1]] {
								for i, vv := range v {
									if vvStr, ok := vv.(string); ok {
										if len(vvStr) > 20 {
											v[i] = vvStr[:20] + "..."
										}
									}
								}
							}
						}
					} else {
						lg.ErrorC("error:", "q", q, "spTo", spTo)
					}
				}
			}
		}
	} else {
		idString = cols[0]
	}

	if dbMem != nil {
		data := map[string]any{
			"dbType":         dbMem.Dialect,
			"table":          model,
			"rows":           rows,
			"dbcolumns":      dbCols,
			"pk":             idString,
			"fkeys":          mmfkeys,
			"fkeysModels":    mmfkeysModels,
			"columnsOrdered": cols,
		}
		if t != nil {
			data["columns"] = t.ModelTypes
		} else {
			data["columns"] = dbCols
		}
		c.Html("admin/admin_single_table.html", data)
	} else {
		lg.ErrorC("table not found", "table", model)
		c.Status(404).Error("Unable to find this model")
	}
}

var AllModelsSearch = func(c *ksmux.Context) {
	model := c.Param("model")
	if model == "" {
		c.Json(map[string]any{
			"error": "Error: No model given in params",
		})
		return
	}

	body := c.BodyJson()

	blder := Table(model).Database(defaultDB)
	if query, ok := body["query"]; ok {
		if v, ok := query.(string); ok {
			if v != "" {
				blder.Where(v)
			}
		} else {
			c.Json(map[string]any{
				"error": "Error: No query given in body",
			})
			return
		}
	}

	oB := ""
	t, err := GetMemoryTable(model, defaultDB)
	if lg.CheckError(err) {
		c.Json(map[string]any{
			"error": err,
		})
		return
	}

	mmfkeysModels := map[string][]map[string]any{}
	mmfkeys := map[string][]any{}
	for _, fkey := range t.Fkeys {
		spFrom := strings.Split(fkey.FromTableField, ".")
		if len(spFrom) == 2 {
			spTo := strings.Split(fkey.ToTableField, ".")
			if len(spTo) == 2 {
				q := "select * from " + spTo[0] + " order by " + spTo[1]
				mm := []map[string]any{}
				err := To(&mm).Query(q)
				if !lg.CheckError(err) {
					ress := []any{}
					for _, res := range mm {
						ress = append(ress, res[spTo[1]])
					}
					if len(ress) > 0 {
						mmfkeys[spFrom[1]] = ress
						mmfkeysModels[spFrom[1]] = mm
						for _, v := range mmfkeysModels[spFrom[1]] {
							for i, vv := range v {
								if vvStr, ok := vv.(string); ok {
									if len(vvStr) > 20 {
										v[i] = vvStr[:20] + "..."
									}
								}
							}
						}
					}
				} else {
					lg.ErrorC("error:", "q", q, "spTo", spTo)
				}
			}
		}
	}

	if oB != "" {
		blder.OrderBy(oB)
	} else {
		blder.OrderBy("-" + t.Pk) // Default order by primary key desc
	}

	// Get page from request body
	pageNum := 1
	if v, ok := body["page_num"]; ok {
		if pn, ok := v.(string); ok {
			if p, err := strconv.Atoi(pn); err == nil {
				pageNum = p
			}
		}
	}
	blder.Limit(paginationPer).Page(pageNum)

	data, err := blder.All()
	if err != nil {
		if err != ErrNoData {
			c.Status(http.StatusBadRequest).Json(map[string]any{
				"error": err.Error(),
			})
			return
		}
		data = []map[string]any{}
	}

	// Get total count for pagination
	var total int64
	var totalRows []int64
	query := "SELECT COUNT(*) FROM " + model
	if v, ok := body["query"]; ok {
		if vStr, ok := v.(string); ok && vStr != "" {
			query += " WHERE " + vStr
		}
	}
	err = To(&totalRows).Query(query)
	if err == nil {
		total = totalRows[0]
	}

	c.Json(map[string]any{
		"table":       model,
		"rows":        data,
		"cols":        t.Columns,
		"types":       t.ModelTypes,
		"fkeys":       mmfkeys,
		"fkeysModels": mmfkeysModels,
		"total":       total,
	})
}

var BulkDeleteRowPost = func(c *ksmux.Context) {
	data := struct {
		Ids   []uint
		Table string
	}{}
	if lg.CheckError(c.BodyStruct(&data)) {
		c.Error("BAD REQUEST")
		return
	}
	idString := "id"
	t, err := GetMemoryTable(data.Table, defaultDB)
	if err != nil {
		c.Status(404).Json(map[string]any{
			"error": "table not found",
		})
		return
	}
	if t.Pk != "" && t.Pk != "id" {
		idString = t.Pk
	}
	_, err = Table(data.Table).Database(defaultDB).Where(idString+" IN (?)", data.Ids).Delete()
	if lg.CheckError(err) {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}
	c.Json(map[string]any{
		"success": "DELETED WITH SUCCESS",
		"ids":     data.Ids,
	})
}

var CreateModelView = func(c *ksmux.Context) {
	data, files := c.ParseMultipartForm()

	model := data["table"][0]
	m := map[string]any{}
	for key, val := range data {
		switch key {
		case "table":
			continue
		case "uuid":
			if v := m[key]; v == "" {
				m[key] = GenerateUUID()
			} else {
				m[key] = val[0]
			}
		case "password":
			hash, _ := argon.Hash(val[0])
			m[key] = hash
		case "email":
			if !IsValidEmail(val[0]) {
				c.Json(map[string]any{
					"error": "email not valid",
				})
				return
			}
			m[key] = val[0]
		case "pk":
			continue
		default:
			if key != "" && val[0] != "" && val[0] != "null" {
				m[key] = val[0]
			}
		}
	}
	inserted, err := Table(model).Database(defaultDB).InsertR(m)
	if err != nil {
		lg.ErrorC("CreateModelView error", "err", err)
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}

	idString := "id"
	t, _ := GetMemoryTable(data["table"][0], defaultDB)
	if t.Pk != "" && t.Pk != "id" {
		idString = t.Pk
	}
	pathUploaded, formName, err := handleFilesUpload(files, data["table"][0], fmt.Sprintf("%v", inserted[idString]), c, idString)
	if err != nil {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}
	if len(pathUploaded) > 0 {
		inserted[formName[0]] = pathUploaded[0]
	}

	c.Json(map[string]any{
		"success":  "Done !",
		"inserted": inserted,
	})
}

var UpdateRowPost = func(c *ksmux.Context) {
	// parse the fkorm and get data values + files
	data, files := c.ParseMultipartForm()
	id := data["row_id"][0]
	idString := "id"
	db, _ := GetMemoryDatabase(defaultDB)
	var t TableEntity
	for _, tab := range db.Tables {
		if tab.Name == data["table"][0] {
			t = tab
		}
	}
	if t.Pk != "" && t.Pk != "id" {
		idString = t.Pk
	}
	_, _, err := handleFilesUpload(files, data["table"][0], id, c, idString)
	if err != nil {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}

	modelDB, err := Table(data["table"][0]).Database(defaultDB).Where(idString+" = ?", id).One()

	if err != nil {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}

	ignored := []string{idString, "file", "image", "photo", "img", "fichier", "row_id", "table"}
	toUpdate := map[string]any{}
	quote := "`"
	if db.Dialect == POSTGRES || db.Dialect == COCKROACH {
		quote = "\""
	}
	for key, val := range data {
		if !SliceContains(ignored, key) {
			if modelDB[key] == val[0] {
				// no changes for bool
				continue
			}
			if key == "password" || key == "pass" {
				hash, err := argon.Hash(val[0])
				if err != nil {
					c.Error("unable to hash pass")
					return
				}
				toUpdate[quote+key+quote] = hash
			} else {
				toUpdate[quote+key+quote] = val[0]
			}
		}
	}

	s := ""
	values := []any{}
	if len(toUpdate) > 0 {
		for col, v := range toUpdate {
			if s == "" {
				s += col + "= ?"
			} else {
				s += "," + col + "= ?"
			}
			values = append(values, v)
		}
	}
	if s != "" {
		_, err := Table(data["table"][0]).Database(defaultDB).Where(idString+" = ?", id).Set(s, values...)
		if err != nil {
			c.Status(http.StatusBadRequest).Json(map[string]any{
				"error": err.Error(),
			})
			return
		}
	}
	s = ""
	if len(files) > 0 {
		for f := range files {
			if s == "" {
				s += f
			} else {
				s += "," + f
			}
		}
	}
	if len(toUpdate) > 0 {
		for k := range toUpdate {
			if s == "" {
				s += k
			} else {
				s += "," + k
			}
		}
	}

	ret, err := Table(data["table"][0]).Database(defaultDB).Where(idString+" = ?", id).One()
	if err != nil {
		c.Status(500).Error("something wrong happened")
		return
	}

	c.Json(map[string]any{
		"success": ret,
	})
}

var TracingGetView = func(c *ksmux.Context) {
	c.Html("admin/admin_tracing.html", nil)
}

var TerminalGetView = func(c *ksmux.Context) {
	c.Html("admin/admin_terminal.html", nil)
}

// WebSocket endpoint for terminal
var TerminalExecute = func(c *ksmux.Context) {
	var req struct {
		Command string `json:"command"`
		Session string `json:"session"`
	}
	if err := c.BodyStruct(&req); err != nil {
		c.Json(map[string]any{"type": "error", "content": err.Error()})
		return
	}

	currentDir, _ := termsessions.Get(req.Session)
	if currentDir == "" {
		currentDir, _ = os.Getwd()
	}

	output, newDir := executeCommand(req.Command, currentDir)

	// Always update the session with the new directory
	termsessions.Set(req.Session, newDir)
	lg.Debug("Updated session directory:", newDir) // Debug log

	c.Json(map[string]any{
		"type":      "output",
		"content":   output,
		"directory": newDir,
	})
}

var GetTraces = func(c *ksmux.Context) {
	dbtraces := GetDBTraces()
	if len(dbtraces) > 0 {
		for _, t := range dbtraces {
			sp, _ := ksmux.StartSpan(context.Background(), t.Query)
			sp.SetTag("query", t.Query)
			sp.SetTag("args", fmt.Sprint(t.Args))
			if t.Database != "" {
				sp.SetTag("database", t.Database)
			}
			sp.SetTag("duration", t.Duration.String())
			sp.SetDuration(t.Duration)
			sp.SetError(t.Error)
			sp.End()
		}
		ClearDBTraces()
	}

	traces := ksmux.GetTraces()
	traceList := make([]map[string]interface{}, 0)
	for traceID, spans := range traces {
		spanList := make([]map[string]interface{}, 0)
		for _, span := range spans {
			errorMsg := ""
			if span.Error() != nil {
				errorMsg = span.Error().Error()
			}
			spanList = append(spanList, map[string]interface{}{
				"id":         span.SpanID(),
				"parentID":   span.ParentID(),
				"name":       span.Name(),
				"startTime":  span.StartTime(),
				"endTime":    span.EndTime(),
				"duration":   span.Duration().String(),
				"tags":       span.Tags(),
				"statusCode": span.StatusCode(),
				"error":      errorMsg,
			})
		}
		traceList = append(traceList, map[string]interface{}{
			"traceID": traceID,
			"spans":   spanList,
		})
	}
	c.Json(traceList)
}

var ClearTraces = func(c *ksmux.Context) {
	ksmux.ClearTraces()
	c.Success("traces cleared")
}

func handleFilesUpload(files map[string][]*multipart.FileHeader, model string, id string, c *ksmux.Context, pkKey string) (uploadedPath []string, formName []string, err error) {
	if len(files) > 0 {
		for key, val := range files {
			file, _ := val[0].Open()
			defer file.Close()
			uploadedImage, err := uploadMultipartFile(file, val[0].Filename, mediaDir+"/uploads/")
			if err != nil {
				return uploadedPath, formName, err
			}
			row, err := Table(model).Database(defaultDB).Where(pkKey+" = ?", id).One()
			if err != nil {
				return uploadedPath, formName, err
			}
			database_image, okDB := row[key]
			uploadedPath = append(uploadedPath, uploadedImage)
			formName = append(formName, key)
			if database_image == uploadedImage {
				return uploadedPath, formName, errors.New("uploadedImage is the same")
			} else {
				if v, ok := database_image.(string); ok || okDB {
					err := c.DeleteFile(v)
					if err != nil {
						//le fichier n'existe pas
						_, err := Table(model).Database(defaultDB).Where(pkKey+" = ?", id).Set(key+" = ?", uploadedImage)
						lg.CheckError(err)
						continue
					} else {
						//le fichier existe et donc supprimer
						_, err := Table(model).Database(defaultDB).Where(pkKey+" = ?", id).Set(key+" = ?", uploadedImage)
						lg.CheckError(err)
						continue
					}
				}
			}
		}
	}
	return uploadedPath, formName, nil
}

var DropTablePost = func(c *ksmux.Context) {
	data := c.BodyJson()
	if table, ok := data["table"]; ok && table != "" {
		if t, ok := data["table"].(string); ok {
			_, err := Table(t).Database(defaultDB).Drop()
			if lg.CheckError(err) {
				c.Status(http.StatusBadRequest).Json(map[string]any{
					"error": err.Error(),
				})
				return
			}
		} else {
			c.Status(http.StatusBadRequest).Json(map[string]any{
				"error": "expecting 'table' to be string",
			})
		}
	} else {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": "missing 'table' in body request",
		})
	}
	c.Json(map[string]any{
		"success": fmt.Sprintf("table %s Deleted !", data["table"]),
	})
}

var ExportView = func(c *ksmux.Context) {
	table := c.Param("table")
	if table == "" {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": "no param table found",
		})
		return
	}
	data, err := Table(table).Database(defaultDB).All()
	lg.CheckError(err)

	data_bytes, err := json.Marshal(data)
	lg.CheckError(err)

	c.Download(data_bytes, table+".json")
}

var ExportCSVView = func(c *ksmux.Context) {
	table := c.Param("table")
	if table == "" {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": "no param table found",
		})
		return
	}
	data, err := Table(table).Database(defaultDB).All()
	lg.CheckError(err)
	var buff bytes.Buffer
	writer := csv.NewWriter(&buff)

	cols := []string{}
	tab, _ := GetMemoryTable(table, defaultDB)
	if len(tab.Columns) > 0 {
		cols = tab.Columns
	} else if len(data) > 0 {
		d := data[0]
		for k := range d {
			cols = append(cols, k)
		}
	}

	err = writer.Write(cols)
	lg.CheckError(err)
	for _, sd := range data {
		values := []string{}
		for _, k := range cols {
			switch vv := sd[k].(type) {
			case string:
				values = append(values, vv)
			case bool:
				if vv {
					values = append(values, "true")
				} else {
					values = append(values, "false")
				}
			case int:
				values = append(values, strconv.Itoa(vv))
			case int64:
				values = append(values, strconv.Itoa(int(vv)))
			case uint:
				values = append(values, strconv.Itoa(int(vv)))
			case time.Time:
				values = append(values, vv.String())
			default:
				values = append(values, fmt.Sprintf("%v", vv))
			}

		}
		err = writer.Write(values)
		lg.CheckError(err)
	}
	writer.Flush()
	c.Download(buff.Bytes(), table+".csv")
}

var ImportView = func(c *ksmux.Context) {
	// get table name
	table := c.Request.FormValue("table")
	if table == "" {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": "no table !",
		})
		return
	}
	t, err := GetMemoryTable(table, defaultDB)
	if lg.CheckError(err) {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}
	// upload file and return bytes of file
	fname, dataBytes, err := c.UploadFile("thefile", "backup", "json", "csv")
	if lg.CheckError(err) {
		c.Status(http.StatusBadRequest).Json(map[string]any{
			"error": err.Error(),
		})
		return
	}
	isCsv := strings.HasSuffix(fname, ".csv")

	// get old data and backup
	modelsOld, _ := Table(table).Database(defaultDB).All()
	if len(modelsOld) > 0 {
		modelsOldBytes, err := json.Marshal(modelsOld)
		if !lg.CheckError(err) {
			_ = os.MkdirAll(mediaDir+"/backup/", 0770)
			dst, err := os.Create(mediaDir + "/backup/" + table + "-" + time.Now().Format("2006-01-02") + ".json")
			lg.CheckError(err)
			defer dst.Close()
			_, err = dst.Write(modelsOldBytes)
			lg.CheckError(err)
		}
	}

	// fill list_map
	list_map := []map[string]any{}
	if isCsv {
		reader := csv.NewReader(bytes.NewReader(dataBytes))
		lines, err := reader.ReadAll()
		if lg.CheckError(err) {
			c.Status(http.StatusBadRequest).Json(map[string]any{
				"error": err.Error(),
			})
			return
		}

		for _, values := range lines {
			m := map[string]any{}
			for i := range values {
				m[t.Columns[i]] = values[i]
			}
			list_map = append(list_map, m)
		}
	} else {
		err := json.Unmarshal(dataBytes, &list_map)
		if lg.CheckError(err) {
			c.Status(http.StatusBadRequest).Json(map[string]any{
				"error": err.Error(),
			})
			return
		}
	}

	// create models in database
	var retErr []error
	for _, m := range list_map {
		_, err = Table(table).Database(defaultDB).Insert(m)
		if err != nil {
			retErr = append(retErr, err)
		}
	}
	if len(retErr) > 0 {
		c.Json(map[string]any{
			"success": "some data could not be added, " + errors.Join(retErr...).Error(),
		})
		return
	}

	c.Json(map[string]any{
		"success": "Import Done , you can see uploaded backups at ./" + mediaDir + "/backup folder",
	})
}

var ManifestView = func(c *ksmux.Context) {
	if embededDashboard {
		f, err := staticAndTemplatesFS[0].ReadFile(staticDir + "/manifest.json")
		if err != nil {
			lg.ErrorC("cannot embed manifest.json", "err", err)
			return
		}
		c.ServeEmbededFile("application/json; charset=utf-8", f)
	} else {
		c.ServeFile("application/json; charset=utf-8", staticDir+"/manifest.json")
	}
}

var ServiceWorkerView = func(c *ksmux.Context) {
	if embededDashboard {
		f, err := staticAndTemplatesFS[0].ReadFile(staticDir + "/sw.js")
		if err != nil {
			lg.ErrorC("cannot embed sw.js", "err", err)
			return
		}
		c.ServeEmbededFile("application/javascript; charset=utf-8", f)
	} else {
		c.ServeFile("application/javascript; charset=utf-8", staticDir+"/sw.js")
	}
}

var RobotsTxtView = func(c *ksmux.Context) {
	c.ServeFile("text/plain; charset=utf-8", "."+staticUrl+"/robots.txt")
}

var OfflineView = func(c *ksmux.Context) {
	c.Text("<h1>YOUR ARE OFFLINE, check connection</h1>")
}

func statsNbRecords() string {
	allTables := GetAllTables(defaultDB)
	q := []string{}
	for _, t := range allTables {
		q = append(q, "SELECT '"+t+"' AS table_name,COUNT(*) AS count FROM "+t)
	}
	query := strings.Join(q, ` UNION ALL `)

	var results []struct {
		TableName string `db:"table_name"`
		Count     int    `db:"count"`
	}
	if err := To(&results).Query(query); lg.CheckError(err) {
		return "0"
	}
	count := 0
	for _, r := range results {
		count += r.Count
	}
	return strconv.Itoa(count)
}

func statsDbSize() string {
	size, err := GetDatabaseSize(defaultDB)
	if err != nil {
		lg.Error(err)
		size = "0 MB"
	}
	return size
}

type LogEntry struct {
	Type  string
	At    string
	Extra string
}

// Global atomic counter for requests
var totalRequests uint64

// GetTotalRequests returns the current total requests count
func GetTotalRequests() uint64 {
	return atomic.LoadUint64(&totalRequests)
}

func parseLogString(logStr string) LogEntry {
	// Handle empty string case
	if logStr == "" {
		return LogEntry{}
	}

	// Split the time from the end
	parts := strings.Split(logStr, "time=")
	timeStr := ""
	mainPart := logStr

	if len(parts) > 1 {
		timeStr = strings.TrimSpace(parts[1])
		mainPart = strings.TrimSpace(parts[0])
	}

	// Get the log type (ERRO, INFO, etc)
	logType := ""
	if len(mainPart) >= 4 {
		logType = strings.TrimSpace(mainPart[:4])
		mainPart = mainPart[4:]
	}

	// Clean up the type
	switch logType {
	case "ERRO":
		logType = "ERROR"
	case "INFO":
		logType = "INFO"
	case "WARN":
		logType = "WARNING"
	case "DEBU":
		logType = "DEBUG"
	case "FATA":
		logType = "FATAL"
	default:
		logType = "N/A"
	}

	return LogEntry{
		Type:  logType,
		At:    timeStr,
		Extra: strings.TrimSpace(mainPart),
	}
}

func reverseSlice[T any](slice []T) []T {
	new := make([]T, 0, len(slice))
	for i := len(slice) - 1; i >= 0; i-- {
		new = append(new, slice[i])
	}
	return new
}

// GetDatabaseSize returns the size of the database in GB or MB
func GetDatabaseSize(dbName string) (string, error) {
	db := databases[0] // default db
	for _, d := range databases {
		if d.Name == dbName {
			db = d
			break
		}
	}

	var size float64
	var err error

	switch db.Dialect {
	case "sqlite", "sqlite3":
		// For SQLite, get the file size
		info, err := os.Stat(dbName + ".sqlite3")
		if err != nil {
			return "0 MB", fmt.Errorf("error getting sqlite db size: %v", err)
		}
		size = float64(info.Size())

	case "postgres", "postgresql":
		// For PostgreSQL, query the pg_database_size function
		var sizeBytes int64
		query := `SELECT pg_database_size($1)`

		err = GetConnection().QueryRow(query, db.Name).Scan(&sizeBytes)
		if err != nil {
			return "0 MB", fmt.Errorf("error getting postgres db size: %v", err)
		}
		size = float64(sizeBytes)

	case "mysql", "mariadb":
		// For MySQL/MariaDB, query information_schema
		var sizeBytes int64
		query := `
			SELECT SUM(data_length + index_length) 
			FROM information_schema.TABLES 
			WHERE table_schema = ?`
		err = GetConnection().QueryRow(query, db.Name).Scan(&sizeBytes)
		if err != nil {
			return "0 MB", fmt.Errorf("error getting mysql db size: %v", err)
		}
		size = float64(sizeBytes)

	default:
		return "0 MB", fmt.Errorf("unsupported database dialect: %s", db.Dialect)
	}

	// Convert bytes to GB (1 GB = 1024^3 bytes)
	sizeGB := size / (1024 * 1024 * 1024)

	// If size is less than 1 GB, convert to MB
	if sizeGB < 1 {
		sizeMB := size / (1024 * 1024)
		return fmt.Sprintf("%.2f MB", sizeMB), nil
	}

	return fmt.Sprintf("%.2f GB", sizeGB), nil
}

func uploadMultipartFile(file multipart.File, filename string, outPath string, acceptedFormats ...string) (string, error) {
	//create destination file making sure the path is writeable.
	if outPath == "" {
		outPath = mediaDir + "/uploads/"
	} else {
		if !strings.HasSuffix(outPath, "/") {
			outPath += "/"
		}
	}
	err := os.MkdirAll(outPath, 0770)
	if err != nil {
		return "", err
	}

	l := []string{"jpg", "jpeg", "png", "json"}
	if len(acceptedFormats) > 0 {
		l = acceptedFormats
	}

	if strings.ContainsAny(filename, strings.Join(l, "")) {
		dst, err := os.Create(outPath + filename)
		if err != nil {
			return "", err
		}
		defer dst.Close()

		//copy the uploaded file to the destination file
		if _, err := io.Copy(dst, file); err != nil {
			return "", err
		} else {
			url := "/" + outPath + filename
			return url, nil
		}
	} else {
		return "", fmt.Errorf("not in allowed extensions 'jpg','jpeg','png','json' : %v", l)
	}
}

// TERMINAL

func executeCommand(command, currentDir string) (output, newDir string) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "", currentDir
	}

	// Check if command is allowed
	if !terminalAllowedCommands[parts[0]] {
		return fmt.Sprintf("Command '%s' not allowed. Use only: %v\n",
			parts[0], getAllowedCommands()), currentDir
	}

	// Handle built-in cd command since it affects the terminal's working directory
	switch parts[0] {
	case "touch":
		if len(parts) < 2 {
			return "Error: missing file name\n", currentDir
		}
		fileName := parts[1]
		filePath := filepath.Join(currentDir, fileName)
		// check if file exists
		if _, err := os.Stat(filePath); err == nil {
			return fmt.Sprintf("File '%s' already exists\n", fileName), currentDir
		}
		_, err := os.Create(filePath)
		if err != nil {
			return fmt.Sprintf("Error creating file: %s\n", err), currentDir
		}
		return fmt.Sprintf("File '%s' created at '%s'\n", fileName, filePath), currentDir
	case "ls", "dir":
		// Default to current directory if no argument provided
		targetDir := currentDir

		// If argument provided, resolve the path
		if len(parts) > 1 {
			// Handle relative or absolute path
			if filepath.IsAbs(parts[1]) {
				targetDir = parts[1]
			} else {
				targetDir = filepath.Join(currentDir, parts[1])
			}
		}

		// Clean up path and check if directory exists
		targetDir = filepath.Clean(targetDir)
		if fi, err := os.Stat(targetDir); err != nil || !fi.IsDir() {
			return fmt.Sprintf("Error: cannot access '%s': No such directory\n", parts[1]), currentDir
		}

		files, err := os.ReadDir(targetDir)
		if err != nil {
			return fmt.Sprintf("Error reading directory: %s\n", err), currentDir
		}

		var output strings.Builder
		for _, file := range files {
			info, err := file.Info()
			if err != nil {
				continue
			}
			prefix := "F"
			if file.IsDir() {
				prefix = "D"
			}
			size := fmt.Sprintf("%8d", info.Size())
			name := file.Name()
			output.WriteString(fmt.Sprintf("[%s] %s %s\n", prefix, size, name))
		}
		return output.String(), currentDir
	case "cd":
		if len(parts) < 2 {
			// cd without args goes to home directory
			home, err := os.UserHomeDir()
			if err != nil {
				return "Error getting home directory: " + err.Error() + "\n", currentDir
			}
			return home, home
		}
		newDir := parts[1]
		if !filepath.IsAbs(newDir) {
			newDir = filepath.Join(currentDir, newDir)
		}
		if fi, err := os.Stat(newDir); err == nil && fi.IsDir() {
			return newDir, newDir
		}
		return "Error: Not a directory\n", currentDir
	case "clear", "cls":
		return "CLEAR", currentDir
	case "pwd":
		return currentDir + "\n", currentDir
	case "vim", "vi", "nano", "nvim":
		return fmt.Sprintf("Interactive editors like %s are not supported in web terminal\n", parts[0]), currentDir
	case "tail":
		if len(parts) < 2 {
			return "Error: missing file name\n", currentDir
		}
		fileName := parts[1]
		filePath := filepath.Join(currentDir, fileName)

		// Default settings
		numLines := 10
		follow := false

		// Parse flags
		for i := 2; i < len(parts); i++ {
			switch parts[i] {
			case "-n":
				if i+1 < len(parts) {
					if n, err := strconv.Atoi(parts[i+1]); err == nil {
						numLines = n
						i++
					}
				}
			case "-f":
				follow = true
			}
		}

		if follow {
			return "tail -f not supported in web terminal (requires WebSocket)\n", currentDir
		}

		// Check file exists
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Sprintf("Error: file '%s' does not exist\n", fileName), currentDir
		}

		// Read file
		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Sprintf("Error reading file: %s\n", err), currentDir
		}

		// Split into lines and get last N lines
		lines := strings.Split(string(content), "\n")
		start := len(lines) - numLines
		if start < 0 {
			start = 0
		}

		return strings.Join(lines[start:], "\n"), currentDir
	case "rmdir":
		if len(parts) < 2 {
			return "Error: missing directory name\n", currentDir
		}
		dirName := parts[1]
		dirPath := filepath.Join(currentDir, dirName)
		if _, err := os.Stat(dirPath); err != nil {
			return fmt.Sprintf("Error: directory '%s' does not exist\n", dirName), currentDir
		}
		if err := os.RemoveAll(dirPath); err != nil {
			return fmt.Sprintf("Error removing directory: %s\n", err), currentDir
		}
		return fmt.Sprintf("Directory '%s' removed\n", dirName), currentDir
	case "rm":
		if len(parts) < 2 {
			return "Error: missing file name\n", currentDir
		}
		fileName := parts[1]
		filePath := filepath.Join(currentDir, fileName)
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Sprintf("Error: file '%s' does not exist\n", fileName), currentDir
		}
		if err := os.Remove(filePath); err != nil {
			return fmt.Sprintf("Error removing file: %s\n", err), currentDir
		}
		return fmt.Sprintf("File '%s' removed\n", fileName), currentDir
	case "cp":
		if len(parts) < 3 {
			return "Error: missing source and destination file names\n", currentDir
		}
		sourceFileName := parts[1]
		destinationFileName := parts[2]
		sourceFilePath := filepath.Join(currentDir, sourceFileName)
		destinationFilePath := filepath.Join(currentDir, destinationFileName)
		if stat, err := os.Stat(sourceFilePath); err != nil {
			return fmt.Sprintf("Error: file '%s' does not exist\n", sourceFileName), currentDir
		} else {
			if stat.IsDir() {
				if err := copyDir(sourceFilePath, destinationFilePath); err != nil {
					return fmt.Sprintf("Error copying directory: %s\n", err), currentDir
				}
				return fmt.Sprintf("Directory '%s' copied to '%s'\n", sourceFileName, destinationFileName), currentDir
			}
		}
		if err := copyFile(sourceFilePath, destinationFilePath); err != nil {
			return fmt.Sprintf("Error copying file: %s\n", err), currentDir
		}
		return fmt.Sprintf("File '%s' copied to '%s'\n", sourceFileName, destinationFileName), currentDir
	case "mv":
		if len(parts) < 3 {
			return "Error: missing source and destination file names\n", currentDir
		}
		sourceFileName := parts[1]
		destinationFileName := parts[2]
		sourceFilePath := filepath.Join(currentDir, sourceFileName)
		destinationFilePath := filepath.Join(currentDir, destinationFileName)
		if _, err := os.Stat(sourceFilePath); err != nil {
			return fmt.Sprintf("Error: file '%s' does not exist\n", sourceFileName), currentDir
		}
		if err := os.Rename(sourceFilePath, destinationFilePath); err != nil {
			return fmt.Sprintf("Error renaming file: %s\n", err), currentDir
		}
		return fmt.Sprintf("File '%s' renamed to '%s'\n", sourceFileName, destinationFileName), currentDir
	case "cat":
		if len(parts) < 2 {
			return "Error: missing file name\n", currentDir
		}
		fileName := parts[1]
		filePath := filepath.Join(currentDir, fileName)
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Sprintf("Error: file '%s' does not exist\n", fileName), currentDir
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Sprintf("Error reading file: %s\n", err), currentDir
		}
		return string(content), currentDir
	case "echo":
		if len(parts) < 2 {
			return "Error: missing text to echo\n", currentDir
		}
		text := strings.Join(parts[1:], " ")
		return text + "\n", currentDir
	case "exit":
		return "EXIT", currentDir
	}

	// Rest of shell commands
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", command)
	} else {
		cmd = exec.Command("/bin/sh", "-c", command)
	}

	cmd.Dir = currentDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("Error: %s\n%s", err, string(out)), currentDir
	}
	return string(out), currentDir
}

func getAllowedCommands() []string {
	cmds := make([]string, 0, len(terminalAllowedCommands))
	for cmd := range terminalAllowedCommands {
		cmds = append(cmds, cmd)
	}
	sort.Strings(cmds)
	return cmds
}

var TerminalComplete = func(c *ksmux.Context) {
	input := c.Request.URL.Query().Get("input")
	session := c.Request.URL.Query().Get("session")

	currentDir, _ := termsessions.Get(session)
	if currentDir == "" {
		currentDir, _ = os.Getwd()
	}

	// Get the last word of the input (after any spaces)
	parts := strings.Fields(input)
	if len(parts) == 0 {
		c.Json(map[string]any{"suggestions": []string{}})
		return
	}

	lastWord := parts[len(parts)-1]
	targetDir := currentDir

	// Handle path completion
	if strings.Contains(lastWord, "/") {
		// Split the path into parts
		pathParts := strings.Split(lastWord, "/")
		// The last part is what we're trying to complete
		searchPattern := pathParts[len(pathParts)-1]
		// Everything before the last part is the directory to search in
		searchDir := strings.Join(pathParts[:len(pathParts)-1], "/")

		// Resolve the full path of the directory to search
		if filepath.IsAbs(searchDir) {
			targetDir = searchDir
		} else {
			targetDir = filepath.Join(currentDir, searchDir)
		}

		// Get all files in the target directory
		files, err := os.ReadDir(targetDir)
		if err != nil {
			lg.Error("Error reading directory:", err)
			c.Json(map[string]any{"suggestions": []string{}})
			return
		}

		// Find matches and build full paths
		suggestions := []string{}
		for _, file := range files {
			name := file.Name()
			if strings.HasPrefix(strings.ToLower(name), strings.ToLower(searchPattern)) {
				if file.IsDir() {
					name += "/"
				}
				// Reconstruct the full path suggestion
				suggestion := strings.Join([]string{searchDir, name}, "/")
				suggestions = append(suggestions, suggestion)
			}
		}

		c.Json(map[string]any{"suggestions": suggestions})
		return
	}

	// Handle non-path completion (first level)
	files, err := os.ReadDir(targetDir)
	if err != nil {
		lg.Error("Error reading directory:", err)
		c.Json(map[string]any{"suggestions": []string{}})
		return
	}

	suggestions := []string{}
	for _, file := range files {
		name := file.Name()
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(lastWord)) {
			if file.IsDir() {
				name += "/"
			}
			suggestions = append(suggestions, name)
		}
	}

	c.Json(map[string]any{"suggestions": suggestions})
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

func copyDir(src, dst string) error {
	err := os.MkdirAll(dst, os.ModePerm)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err = copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err = copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

var GetMetricsView = func(c *ksmux.Context) {
	metrics := GetSystemMetrics()
	c.Json(metrics)
}

var GetLogsView = func(c *ksmux.Context) {
	parsed := make([]LogEntry, 0)
	if v := lg.GetLogs(); v != nil {
		for _, vv := range reverseSlice(v.Slice) {
			parsed = append(parsed, parseLogString(vv))
		}
	}
	c.Json(parsed)
}
