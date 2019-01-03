package auth

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/appbaseio-confidential/arc/model/category"
	"github.com/appbaseio-confidential/arc/model/credential"
	"github.com/appbaseio-confidential/arc/model/permission"
	"github.com/appbaseio-confidential/arc/model/user"
	"github.com/appbaseio-confidential/arc/util"
	"github.com/gorilla/mux"
)

// BasicAuth middleware that authenticates each requests against the basic auth credentials.
func (a *Auth) Authorize(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		username, password, ok := req.BasicAuth()
		if !ok {
			util.WriteBackError(w, "Not logged in", http.StatusUnauthorized)
			return
		}

		var (
			reqCredential credential.Credential
			reqUser       *user.User
			reqPermission *permission.Permission
			err           error
		)

		// TODO: Temporary
		// if the provided credentials are from .env file
		// we simply ignore the rest of the checks and serve
		// the request.
		reqUser, err = a.isMaster(ctx, username, password)
		if err != nil {
			log.Printf("%s: %v", logTag, err)
			util.WriteBackMessage(w, "Unable create a master user", http.StatusInternalServerError)
			return
		}
		if reqUser != nil {
			ctx := req.Context()
			ctx = context.WithValue(ctx, credential.CtxKey, credential.User)
			ctx = context.WithValue(ctx, user.CtxKey, reqUser)
			req = req.WithContext(ctx)
			h(w, req)
			return
		}

		obj, err := a.es.getCredential(req.Context(), username, password)
		if err != nil {
			log.Printf("%s: %v\n", logTag, err)
			util.WriteBackError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if obj == nil {
			msg := fmt.Sprintf(`Credential with "username"="%s" Not Found`, username)
			util.WriteBackError(w, msg, http.StatusNotFound)
			return
		}

		reqPermission, ok = obj.(*permission.Permission)
		if ok {
			reqCredential = credential.Permission
		} else {
			reqUser, ok = obj.(*user.User)
			if ok {
				reqCredential = credential.User
			} else {
				msg := fmt.Sprintf(`Credential with "username"="%s" Not Found`, username)
				log.Printf(`%s: cannot cast obj "%v" to either permission.Permission or user.User`, logTag, obj)
				util.WriteBackError(w, msg, http.StatusNotFound)
				return
			}
		}

		reqCategory, err := category.FromContext(ctx)
		if err != nil {
			msg := "error occurred while authenticating the request"
			log.Printf("%s: %v", logTag, err)
			util.WriteBackError(w, msg, http.StatusInternalServerError)
			return
		}

		if reqCategory.IsFromES() {
			if reqCredential == credential.User {
				if !(*reqUser.IsAdmin) {
					msg := fmt.Sprintf(`User with "username"="%s" is not an admin`, username)
					util.WriteBackError(w, msg, http.StatusUnauthorized)
					return
				}
				if password != reqUser.Password {
					util.WriteBackError(w, "Incorrect credentials", http.StatusUnauthorized)
					return
				}
				ctx := req.Context()
				ctx = context.WithValue(ctx, credential.CtxKey, reqCredential)
				ctx = context.WithValue(ctx, user.CtxKey, reqUser)
				req = req.WithContext(ctx)
			} else {
				if password != reqPermission.Password {
					util.WriteBackMessage(w, "Incorrect credentials", http.StatusUnauthorized)
					return
				}
				ctx := req.Context()
				ctx = context.WithValue(ctx, credential.CtxKey, reqCredential)
				ctx = context.WithValue(ctx, permission.CtxKey, reqPermission)
				req = req.WithContext(ctx)
			}
		} else {
			// if we are patching a user or a permission, we must clear their
			// respective objects from the cache, otherwise the changes won't be
			// reflected the next time user tries to get the user or permission object.
			if req.Method == http.MethodPatch || req.Method == http.MethodDelete {
				switch *reqCategory {
				case category.User:
					a.removeUserFromCache(username)
				case category.Permission:
					username := mux.Vars(req)["username"]
					a.removePermissionFromCache(username)
				}
			}

			// check in the cache
			reqUser, ok = a.cachedUser(username)
			if !ok {
				reqUser, err = a.es.getUser(req.Context(), username)
				if err != nil {
					msg := fmt.Sprintf(`User with "user_id"="%s" Not Found`, username)
					log.Printf("%s: %s: %v", logTag, msg, err)
					util.WriteBackError(w, msg, http.StatusNotFound)
					return
				}
				// store in the cache
				a.cacheUser(username, reqUser)
			}

			if password != reqUser.Password {
				util.WriteBackMessage(w, "Incorrect credentials", http.StatusUnauthorized)
				return
			}

			ctx := req.Context()
			ctx = context.WithValue(ctx, user.CtxKey, reqUser)
			req = req.WithContext(ctx)
		}

		h(w, req)
	}
}


func (a *Auth) isMaster(ctx context.Context, username, password string) (*user.User, error) {
	masterUser, masterPassword := os.Getenv("USERNAME"), os.Getenv("PASSWORD")
	if masterUser != username || masterPassword != password {
		return nil, nil
	}

	master, err := a.es.getUser(ctx, username)
	if err != nil {
		log.Printf("%s: master user doesn't exists, creating one... : %v", logTag, err)
		master, err = user.NewAdmin(masterUser, masterPassword)
		if err != nil {
			return nil, err
		}
		ok, err := a.es.putUser(ctx, *master)
		if !ok || err != nil {
			return nil, fmt.Errorf("%s: unable to create master user: %v", logTag, err)
		}
	}

	return master, nil
}
