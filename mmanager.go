package mtproto

import (
	"log"
	"os"
	"sync"
	"fmt"
	"math/rand"
	"time"
	"encoding/json"
)

type MManager struct {
	managerId 	int32
	appConfig	Configuration
	conns		map[int32]*MConn
	sessions  	map[int64]*MSession
	eventq    	chan MEvent

	manageInterrupter chan struct{}
	manageWaitGroup   sync.WaitGroup
}

const (
	// Current API Layer Version
	layer = 65

	// API Errors
	errorSeeOther     = 303
	errorBadRequest   = 400
	errorUnauthorized = 401
	errorForbidden    = 403
	errorNotFound     = 404
	errorFlood        = 420
	errorInternal     = 500
)

func NewManager (appConfig Configuration) (*MManager, error) {
	var err error

	err = appConfig.Check()
	if err != nil {
		return nil, err
	}

	mm := new(MManager)
	rand.Seed(time.Now().UnixNano())
	mm.managerId = rand.Int31()
	mm.appConfig = appConfig
	//TODO: set proper buf size to channels
	mm.conns = make(map[int32]*MConn)
	mm.sessions = make(map[int64]*MSession)
	mm.eventq = make(chan MEvent)
	mm.manageInterrupter = make(chan struct{})
	mm.manageWaitGroup = sync.WaitGroup{}

	go mm.manageRoutine()

	return mm, nil
}

func (mm *MManager) Finish() {
	// close all connections
	for id, _ := range mm.conns {
		mm.eventq <- closeConnection{id, nil}
	}

	// Send stop signal to manage routine
	close(mm.manageInterrupter)

	// Wait for event routines + manage routine
	mm.manageWaitGroup.Wait()
}

func (mm *MManager) IsAuthenticated(phonenumber string) bool {
	sessionfile := sessionFilePath(mm.appConfig.SessionHome, phonenumber)
	_, err := os.Stat(sessionfile)
	if os.IsNotExist(err) {
		return false
	}
	return true
}

func (mm *MManager) LoadAuthentication(phonenumber string, preferredAddr string) (*MConn, error) {
	// req connect
	respCh := make(chan sessionResponse)
	mm.eventq  <- loadsession{0, phonenumber, preferredAddr, respCh}

	// Wait for connection built
	resp := <- respCh
	if resp.err != nil {
		return nil, resp.err
	}

	// Check user authentication by user info
	mconn := mm.conns[resp.connId]
	//state, err := mconn.UpdatesGetState()
	//if err != nil {
	//	return nil, err
	//}

	userFull, err := mconn.UsersGetFullUsers(TL_inputUserSelf{})
	if err != nil {
		// Need to authenticate
		return nil, err
	}

	// Already authenticated
	user := userFull.User.(TL_user)
	session, err := mconn.Session()
	if err != nil {
		return mconn, err
	}
	session.user = &user
	logln(mm, "Auth as ", user)
	return mm.conns[resp.connId], nil
}

func (mm *MManager) NewAuthentication(phonenumber string, addr string, useIPv6 bool) (*MConn, *TL_auth_sentCode, error) {
	// req connect
	respCh := make(chan sessionResponse)
	mm.eventq <- newsession{0, phonenumber, addr, useIPv6, respCh}

	// Wait for connection
	resp := <- respCh
	if resp.err != nil {
		return nil, nil, resp.err
	}

	// sendAuthCode
	mconn := mm.conns[resp.connId]
	mconn, sentCode, err := mm.authSendCode(mconn, phonenumber)
	if err != nil {
		return nil, nil, err
	}

	return mconn, sentCode, nil
}

func (mm *MManager) manageRoutine() {
	logln(mm, "start")
	mm.manageWaitGroup.Add(1)
	defer mm.manageWaitGroup.Done()

	for {
		select {
		case <-mm.manageInterrupter:
			// Default interrupt is STOP
			logln(mm, "stop")
			return

		case e := <-mm.eventq :
			// Delegate event handlings to go routines
			switch e.(type) {
			// Session Event Handlers
			// In normal case, three resp events,
			// SessionEstablished, ConnectionOpened, sessionBound,
			// are generated and propagated.
			case newsession:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(newsession)
					logln(mm, "newsession to ", e.addr)
					session, err := newSession(e.phonenumber, e.addr, e.useIPv6, mm.appConfig, mm.eventq)
					if err != nil {
						//log.Fatalln("ManageRoutine: Connect Failure", err)
						fatalln(mm, "connect failure: ", err)
						//TODO: need to handle nil resp channel?
						e.resp <- sessionResponse{0, nil, err}
					} else {
						// Bind the session with mconn and mmanager
						mm.sessions[session.sessionId] = session	// Immediate registration
						var mconn *MConn
						if e.connId != 0 {
							mconn = mm.conns[e.connId]
						} else {
							// Create new connection, if not exist
							mconn, err = newConnection(mm.eventq)
							if err != nil {
								e.resp <- sessionResponse{0, nil, err}
								return
							}
							mm.conns[mconn.connId] = mconn	// Immediate registration
						}
						mconn.bind(session)
						//TODO: need to handle nil resp channel?
						e.resp <- sessionResponse{mconn.connId, session, nil}
					}
				}()

			// In normal case, three resp events,
			// SessionEstablished, ConnectionOpened, sessionBound,
			// are generated and propagated.
			case loadsession:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(loadsession)
					logln(mm, "loadsession of ", e.phonenumber)
					session, err := loadSession(e.phonenumber, e.preferredAddr, mm.appConfig, mm.eventq)
					if err != nil {
						//log.Fatalln("ManageRoutine: Connect Failure", err)
						fatalln(mm, "connect failure ", err)
						//TODO: need to handle nil resp channel?
						e.resp <- sessionResponse{0, nil, err}
					} else {
						// Bind the session with mconn and mmanager
						mm.sessions[session.sessionId] = session	// Immediate registration
						var mconn *MConn
						if e.connId != 0 {
							mconn = mm.conns[e.connId]
						} else {
							mconn, err = newConnection(mm.eventq)
							if err != nil {
								e.resp <- sessionResponse{0, nil, err}
								return
							}
							mm.conns[mconn.connId] = mconn	// Immediate registration
						}
						mconn.bind(session)
						//TODO: need to handle nil resp channel?
						e.resp <- sessionResponse{mconn.connId, session, nil}
					}
				}()

			case SessionEstablished:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(SessionEstablished)
					logf(mm, "session established %d\n\n", e.session.sessionId)
				}()

			// In normal case, an event,
			// SessionDiscarded,
			// is generated and propagated.
			case discardSession:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(discardSession)
					logln(mm, "discard session ", e.sessionId)
					session := mm.sessions[e.sessionId]
					session.close()

					// Immediate assignment of discarded session's updates state
					// The assignment on handling SessionDiscarded event is sometimes slower than new sessionBound
					// event, so that it results in either nil discardedUpdateState or a lot of duplicated updates.
					marshaled, err := json.Marshal(session.updatesState)
					if err == nil {
						logf(mm, "session is discarded. keep its updates state, (json): %s\n", marshaled)
					} else {
						logf(mm, "session is discarded. keep its updates state, %v\n", session.updatesState)
					}
					mconn := mm.conns[e.connId]
					mconn.discardedUpdatesState = new(TL_updates_state)
					*mconn.discardedUpdatesState = *session.updatesState
					e.resp <- sessionResponse{e.connId, session, nil}
				}()

			case SessionDiscarded:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(SessionDiscarded)
					logln(mm, "session discarded ", e.discardedSessionId)
					delete(mm.sessions, e.discardedSessionId)	// Late deregistration
				}()

			// In normal case, five events,
			// discardSesseion, (SessionDiscarded), newsession, (SessionEstablished, ConnectionOpened, sessionBound),
			// are generated and propagated.
			case renewSession:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(renewSession)
					logln(mm, "renewSession to ", e.addr)
					connId := mm.sessions[e.sessionId].connId

					// Req discardSession
					disconnectRespCh := make(chan sessionResponse)
					//mm.eventq <- discardSession{e.SessionId(), disconnectRespCh}
					mm.sessions[e.sessionId].notify(discardSession{connId, e.sessionId, disconnectRespCh})

					// Wait for disconnection
					disconnectResp := <- disconnectRespCh
					if disconnectResp.err != nil {
						logf(mm, "renewSession failure: cannot discardSession %d. %v\n", e.sessionId, disconnectResp.err)
						e.resp <- sessionResponse{0, nil, fmt.Errorf("cannot discardSession %d. %v", e.sessionId, disconnectResp.err)}
						return
					}

					// Req newsession
					logln(mm, "renewRoutine: req newsession")
					connectRespCh := make(chan sessionResponse)
					mm.eventq <- newsession{connId, e.phonenumber, e.addr, e.useIPv6, connectRespCh}
					connectResp := <-connectRespCh
					if connectResp.err != nil {
						logf(mm, "renewSession failure: cannot connect to %s. %v\n", e.addr, connectResp.err)
						e.resp <- sessionResponse{0, nil, fmt.Errorf("cannot connect to %s. %v", e.addr, connectResp.err)}
						return
					}
					//TODO: need to handle nil resp channel?
					e.resp <- sessionResponse{connectResp.connId, connectResp.session, nil}
					//TODO: figure out missed updates
					logln(mm, "renewSession done")
				}()

			// In normal case, five events,
			// discardSesseion, (SessionDiscarded), newsession, (SessionEstablished, ConnectionOpened, sessionBound),
			// are generated and propagated.
			case refreshSession:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(refreshSession)
					logln(mm, "refreshSession ", e.sessionId)
					//TODO: alternate the spin lock
					// Wait for session registration and binding for graceful refreshing
					spinLock := false
					if mm.sessions[e.sessionId] == nil {
						spinLock = true
					}
					for spinLock {
						select {
						case <-time.After(1 * time.Second):
							if mm.sessions[e.sessionId] != nil && mm.sessions[e.sessionId].connId != 0{
								spinLock = false
							}
						}
					}
					connId := mm.sessions[e.sessionId].connId

					// Req discardSession
					disconnectRespCh := make(chan sessionResponse)
					//mm.eventq <- discardSession{e.SessionId(), disconnectRespCh}
					mm.sessions[e.sessionId].notify(discardSession{connId, e.sessionId, disconnectRespCh})

					// Wait for disconnected event
					disconnectResp := <- disconnectRespCh
					if disconnectResp.err != nil {
						logf(mm, "refreshSession failure: cannot discardSession %d. %v\n", e.sessionId, disconnectResp.err)
						return
					}

					// Req loadsession
					logln(mm, "refreshRoutine: req loadsession")
					connectRespCh := make(chan sessionResponse)
					mm.eventq <- loadsession{connId, e.phonenumber, "", connectRespCh}
					connectResp := <- connectRespCh
					if connectResp.err != nil {
						logln(mm, "refreshSession failure: ", connectResp.err)
						return
					}
					//TODO: need to handle nil resp channel?
					e.resp <- sessionResponse{connectResp.connId, connectResp.session, nil}
					//TODO: figure out missed updates
					logln(mm, "refreshSessino done")
				}()

			// Connection Event Handlers
			case ConnectionOpened:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(ConnectionOpened)
					logln(mm, "connectionOpened ", e.mconn.connId)
				}()

			case sessionBound:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(sessionBound)
					connId := e.mconn.connId
					sessionId := e.mconn.session.sessionId
					logf(mm, "sessionBound: session %d is bound to mconn %d\n", sessionId, connId)
				}()
			case sessionUnbound:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(sessionUnbound)
					logf(mm, "sessionUnbound: session %d is unbound from mconn %d\n", e.unboundSessionId, e.mconn.connId)
				}()
			case closeConnection:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(closeConnection)
					logln(mm, "closeConnection ", e.connId)

					// close, unbound, and deregister session
					mconn := mm.conns[e.connId]
					session, err := mconn.Session()
					if err != nil {
						e.resp <- err
						return
					}
					discardSessionRespCh := make(chan sessionResponse)
					//mm.eventq <- discardSession{closeE.connId, session.sessionId, discardSessionRespCh}
					mconn.notify(discardSession{e.connId, session.sessionId, discardSessionRespCh})

					// close and deregister connection
					discardSessionResp := <- discardSessionRespCh
					if discardSessionResp.err == nil {
						mconn.close()
						e.resp <- nil
						return
					}
					logln(mm, "closeConnection failure: cannot discard its session ", session.sessionId)
					e.resp <- fmt.Errorf("Failed to discard its session %d", session.sessionId)
				}()
			case connectionClosed:
				go func() {
					mm.manageWaitGroup.Add(1)
					defer mm.manageWaitGroup.Done()
					e := e.(connectionClosed)
					logln(mm, "connectionClosed ", e.closedConnId)
					delete(mm.conns, e.closedConnId)	// Late deregistration
				}()
			case updateReceived:
			default:
			}
		}
	}
	logln(mm, "done")
}

var logging bool
func EnableLogging() {
	logging = true
}
func DisableLogging() {
	logging = false
}

func logprefix(x interface{}) string {
	switch x.(type) {
	case *MConn:
		x := x.(*MConn)
		return fmt.Sprintf("[mconn %d]", x.connId)
	case *MSession:
		x := x.(*MSession)
		return fmt.Sprintf("[%d-%d]", x.connId, x.sessionId)
	case *MManager:
		x := x.(*MManager)
		return fmt.Sprintf("[MM %d]", x.managerId)
	default:
		return ""
	}
}

func logf(x interface{}, format string, v ...interface{}) {
	if !logging {
		return
	}
	log.Printf(logprefix(x) + " " + format, v...)
}

func logln(x interface{}, v ...interface{}) {
	if !logging {
		return
	}
	if len(v) > 0 {
		log.Println(append([]interface{}{logprefix(x)} , v...)...)
	} else {
		log.Println(logprefix(x))
	}
	//logf(x, format + "\n", v...)
}

func fatalf(x interface{}, format string, v ...interface{}) {
	log.Fatalf(logprefix(x) + " " + format, v...)
}

func fatalln(x interface{}, v ...interface{}) {
	if len(v) > 0 {
		log.Fatalln(append([]interface{}{logprefix(x)}, v...)...)
	} else {
		log.Fatalln(logprefix(x))
	}
}

func errorf(x interface{}, format string, v ...interface{}) error {
	return fmt.Errorf(logprefix(x) + " " + format, v...)
}

func Stringify(x interface{}) string {
	marshaled, err := json.Marshal(x)
	if err == nil {
		return fmt.Sprintf("%T: %s", x, marshaled)
	}
	return fmt.Sprintf("%T: %v:", x, x)
}