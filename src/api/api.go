package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	datastructures "github.com/bbernhard/imagemonkey-playground/datastructures"
	"github.com/garyburd/redigo/redis"
	"github.com/getsentry/raven-go"
	"github.com/gin-gonic/gin"
	"github.com/gofrs/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/yrsh/simplify-go"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func GetEnv(name string) string {
	val, found := os.LookupEnv(name)
	if found {
		return val
	}

	return ""
}

func MustGetEnv(name string) string {
	val := GetEnv(name)
	if val == "" {
		log.Fatal("Couldn't get env ", name)
	}

	return val
}

//CORS Middleware
func CorsMiddleware(allowOrigin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", allowOrigin)
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With, X-PINGOTHER, X-File-Name, Cache-Control")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET,    PUT, PATCH, HEAD")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(200)
		} else {
			c.Next()
		}
	}
}

func main() {
	log.SetLevel(log.DebugLevel)

	releaseMode := flag.Bool("release", false, "Run in release mode")
	redisAddress := flag.String("redis_address", ":6379", "Address to the Redis server")
	redisMaxConnections := flag.Int("redis_max_connections", 50, "Max connections to Redis")
	predictionsDir := flag.String("predictions_dir", "../predictions/", "Location of the temporary saved images for predictions")
	donationsDir := flag.String("donations_dir", "../../imagemonkey-core/donations/", "Location of the uploaded and verified donations")
	corsAllowOrigin := flag.String("cors_allow_origin", "*", "CORS Access-Control-Allow-Origin")
	listenPort := flag.Int("listen_port", 8082, "Specify the listen port")
	useSentry := flag.Bool("use_sentry", false, "Use Sentry for error logging")

	flag.Parse()
	if *releaseMode {
		fmt.Printf("[Main] Starting gin in release mode!\n")
		gin.SetMode(gin.ReleaseMode)
	}

	if *useSentry {
		fmt.Printf("Setting Sentry DSN\n")
		sentryDsn := MustGetEnv("SENTRY_DSN")
		raven.SetEnvironment("grabcut")
		raven.SetDSN(sentryDsn)

		raven.CaptureMessage("Starting up playground-api worker", nil)
	}

	//creating predictions-dir if it not already exists
	//as predicitions are temporary the directory might not already exist (e.q if predictions are stored in /tmp and server reboots)
	if _, err := os.Stat(*predictionsDir); os.IsNotExist(err) {
		log.Debug("[Main] Creating directory for predictions as it doesn't exist")
		err := os.Mkdir(*predictionsDir, 0755)
		if err != nil {
			log.Debug("[Main] Couldn't create directory: ", err.Error())
			os.Exit(1)
		}
	}

	redisPool := redis.NewPool(func() (redis.Conn, error) {
		c, err := redis.Dial("tcp", *redisAddress)

		if err != nil {
			return nil, err
		}

		return c, err
	}, *redisMaxConnections)
	defer redisPool.Close()

	router := gin.Default()
	router.Use(CorsMiddleware(*corsAllowOrigin))

	/*router.OPTIONS("/v1/predict", func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
	    c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With, X-PINGOTHER, X-File-Name, Cache-Control")
	    c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT")
	    c.JSON(http.StatusOK, struct{}{})
	})*/

	router.POST("/v1/predict", func(c *gin.Context) {
		/*	c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With, X-PINGOTHER, X-File-Name, Cache-Control")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT")*/
		c.Writer.Header().Set("Access-Control-Expose-Headers", "Location")

		classificationType := c.PostForm("classification_type")

		_, header, err := c.Request.FormFile("image")
		if err != nil {
			c.JSON(400, gin.H{"error": "Picture is missing"})
			return
		}

		u, err := uuid.NewV4()
		if err != nil {
			c.JSON(500, gin.H{"error": "Couldn't process request, please try again later!"})
			return
		}

		uuid := u.String()
		c.SaveUploadedFile(header, (*predictionsDir + uuid))

		redisConn := redisPool.Get()
		defer redisConn.Close()

		//add a prediction request to the REDIS 'predictme' queue
		var predictionRequest datastructures.PredictionRequest
		predictionRequest.Uuid = uuid
		predictionRequest.Created = int64(time.Now().Unix())
		predictionRequest.Filename = (*predictionsDir + uuid)

		if classificationType == "nsfw" {
			predictionRequest.Type = "nsfw-classification"
		} else {
			predictionRequest.Type = "classification"
		}

		serialized, err := json.Marshal(predictionRequest)
		if err != nil {
			log.Debug("[Predicting] Couldn't serialize request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't accept request - please try again later"})
			return
		}

		_, err = redisConn.Do("RPUSH", "predictme", serialized)
		if err != nil {
			log.Debug("[Predicting] Couldn't accept request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't accept request - please try again later"})
			return
		}

		c.Writer.Header().Set("Location", uuid)
		c.JSON(202, gin.H{})
	})

	router.GET("/v1/predict/:uuid", func(c *gin.Context) {
		/*c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		  c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With, X-PINGOTHER, X-File-Name, Cache-Control")
		  c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT")*/

		uuid := c.Param("uuid")
		key := "predict" + uuid

		redisConn := redisPool.Get()
		defer redisConn.Close()

		ok, err := redis.Bool(redisConn.Do("EXISTS", key))
		if err != nil {
			log.Debug("[Predicting] Couldn't check status of request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't check status of request - please try again later"})
			return
		}

		if !ok { //nothing available yet. Which means either the uuid is wrong or processing isn't finished.
			//at this point we don't care for the reason.
			c.JSON(200, gin.H{})
			return
		}

		var data []byte
		var predictionResult datastructures.PredictionResult
		data, err = redis.Bytes(redisConn.Do("GET", key))
		if err != nil {
			log.Debug("[Predicting] Couldn't get status of request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't get status of request - please try again later"})
			return
		}

		err = json.Unmarshal(data, &predictionResult)
		if err != nil {
			log.Debug("[Predicting] Couldn't unmarshal: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't get status of request - please try again later"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"label": predictionResult.Result.Label, "score": predictionResult.Result.Score,
			"model_info": predictionResult.ModelInfo})
	})

	router.POST("/v1/grabcut", func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Expose-Headers", "Location")

		redisConn := redisPool.Get()
		defer redisConn.Close()

		file, _, err := c.Request.FormFile("image")
		if err != nil {
			log.Debug("image is missing")
			c.JSON(400, gin.H{"error": "Picture is missing"})
			return
		}

		imageUuid := c.PostForm("uuid")
		if imageUuid == "" {
			c.JSON(422, gin.H{"error": "Couldn't process request - parameters missing"})
			return
		}

		buf := bytes.NewBuffer(nil)
		if _, err := io.Copy(buf, file); err != nil {
			c.JSON(500, gin.H{"error": "Couldn't process request - please try again later"})
			return
		}

		u, err := uuid.NewV4()
		if err != nil {
			c.JSON(500, gin.H{"error": "Couldn't process request - please try again later"})
			return
		}

		var grabcutRequest datastructures.GrabcutRequest
		grabcutRequest.Filename = (*donationsDir + imageUuid)
		grabcutRequest.Mask = buf.Bytes()
		grabcutRequest.Uuid = u.String()

		serialized, err := json.Marshal(grabcutRequest)
		if err != nil {
			log.Debug("[Grabcutme] Couldn't serialize request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't accept request - please try again later"})
			return
		}

		_, err = redisConn.Do("RPUSH", "grabcutme", serialized)
		if err != nil {
			log.Debug("[Grabcutme] Couldn't accept request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't accept request - please try again later"})
			return
		}

		c.Writer.Header().Set("Location", grabcutRequest.Uuid)
		c.JSON(202, gin.H{})
	})

	router.GET("/v1/grabcut/:uuid", func(c *gin.Context) {
		//c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		uuid := c.Param("uuid")
		key := "grabcut" + uuid

		redisConn := redisPool.Get()
		defer redisConn.Close()

		ok, err := redis.Bool(redisConn.Do("EXISTS", key))
		if err != nil {
			log.Debug("[Grabcut] Couldn't check status of request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't check status of request - please try again later"})
			return
		}

		if !ok { //nothing available yet. Which means either the uuid is wrong or processing isn't finished.
			//at this point we don't care for the reason.
			c.JSON(200, gin.H{})
			return
		}

		var data []byte
		var grabcutResult datastructures.GrabcutResult
		data, err = redis.Bytes(redisConn.Do("GET", key))
		if err != nil {
			log.Debug("[Grabcut] Couldn't get status of request: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't get status of request - please try again later"})
			return
		}

		err = json.Unmarshal(data, &grabcutResult)
		if err != nil {
			log.Debug("[Grabcut] Couldn't unmarshal: ", err.Error())
			c.JSON(500, gin.H{"error": "Couldn't get status of request - please try again later"})
			return
		}

		//simplify polyline
		var grabcutMeResult datastructures.GrabcutMeResult
		simplifiedDataPoints := simplifier.Simplify(grabcutResult.Points, 1.5, false)
		for i, _ := range simplifiedDataPoints {
			var item datastructures.GrabcutMeResultPoint
			item.X = float32(simplifiedDataPoints[i][0])
			item.Y = float32(simplifiedDataPoints[i][1])
			grabcutMeResult.Points = append(grabcutMeResult.Points, item)
		}

		grabcutMeResult.Angle = 0
		grabcutMeResult.Type = "polygon"

		if grabcutResult.Error == "" {
			c.JSON(http.StatusOK, gin.H{"result": grabcutMeResult})
		} else {
			c.JSON(http.StatusOK, gin.H{"result": grabcutMeResult, "error": grabcutResult.Error})
		}
	})

	if *corsAllowOrigin == "*" {
		corsWarning := "CORS Access-Control-Allow-Origin is set to '*' - which is a potential security risk."
		corsWarning += "DO NOT RUN THE SERVICE IN PRODUCTION WITH THIS CONFIGURATION!"
		log.Info(corsWarning)
	}

	router.Run(":" + strconv.Itoa(*listenPort))
}
