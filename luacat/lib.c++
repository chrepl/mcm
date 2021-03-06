// Copyright 2016 The Minimal Configuration Manager Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include "luacat/lib.h"

#include <fcntl.h>
#include "kj/debug.h"
#include "kj/exception.h"
#include "kj/string.h"
#include "kj/vector.h"
#include "capnp/message.h"
#include "capnp/orphan.h"
#include "capnp/schema.h"
#include "lua.hpp"
#include "openssl/sha.h"

#include "catalog.capnp.h"
#include "luacat/convert.h"
#include "luacat/types.h"

namespace mcm {

namespace luacat {

namespace {
  const char* idHashPrefix = "mcm-luacat ID: ";
  const char* resourceTypeMetaKey = "mcm_resource";
  const char* stateRefRegistryKey = "mcm::Lua";
  const uint64_t fileResId = 0x8dc4ac52b2962163;
  const uint64_t execResId = 0x984c97311006f1ca;

  LibState& getStateRef(lua_State* state) {
    int ty = lua_getfield(state, LUA_REGISTRYINDEX, stateRefRegistryKey);
    KJ_ASSERT(ty == LUA_TLIGHTUSERDATA);
    auto ptr = reinterpret_cast<LibState*>(lua_touserdata(state, -1));
    lua_pop(state, 1);
    return *ptr;
  }

  uint64_t idHash(kj::StringPtr s) {
    SHA_CTX ctx;
    SHA1_Init(&ctx);
    SHA1_Update(&ctx, idHashPrefix, strlen(idHashPrefix));
    SHA1_Update(&ctx, s.cStr(), s.size());
    uint8_t hash[SHA_DIGEST_LENGTH];
    SHA1_Final(hash, &ctx);
    return 1 | hash[0] |
        (((uint64_t)hash[1]) << 8) |
        (((uint64_t)hash[2]) << 16) |
        (((uint64_t)hash[3]) << 24) |
        (((uint64_t)hash[4]) << 32) |
        (((uint64_t)hash[5]) << 40) |
        (((uint64_t)hash[6]) << 48) |
        (((uint64_t)hash[7]) << 56);
  }

  int hashfunc(lua_State* state) {
    if (lua_gettop(state) != 1) {
      return luaL_error(state, "'mcm.hash' takes 1 argument, got %d", lua_gettop(state));
    }
    luaL_argcheck(state, lua_isstring(state, 1), 1, "must be a string");
    auto comment = luaStringPtr(state, 1);
    pushId(state, kj::heap<Id>(idHash(comment), comment));
    return 1;
  }

  void setResourceType(lua_State* state, int index, uint64_t val) {
    if (index < 0) {
      index = lua_gettop(state) + index + 1;
    }

    // Create metatable and leave it at top of stack.
    lua_createtable(state, 0, 1);
    if (lua_getmetatable(state, index)) {
      // If there was an existing metatable, then setmetatable(newmeta, {__index = oldmeta})
      lua_createtable(state, 0, 1);
      lua_pushvalue(state, -2);  // move oldmeta to top
      lua_setfield(state, -2, "__index");
      lua_setmetatable(state, -3);
      lua_pop(state, 1);  // pop oldmeta
    }

    // metatable[resourceTypeMetaKey] = resourceType(val)
    pushResourceType(state, val);
    lua_setfield(state, -2, resourceTypeMetaKey);

    lua_setmetatable(state, index);
  }

  int filefunc(lua_State* state) {
    if (lua_gettop(state) != 1) {
      return luaL_error(state, "'mcm.file' takes 1 argument, got %d", lua_gettop(state));
    }
    luaL_argcheck(state, lua_istable(state, 1), 1, "must be a table");
    setResourceType(state, 1, fileResId);
    return 1;  // Return original argument
  }

  int execfunc(lua_State* state) {
    if (lua_gettop(state) != 1) {
      return luaL_error(state, "'mcm.exec' takes 1 argument, got %d", lua_gettop(state));
    }
    luaL_argcheck(state, lua_istable(state, 1), 1, "must be a table");
    setResourceType(state, 1, execResId);
    return 1;  // Return original argument
  }

  int resourcefunc(lua_State* state) {
    if (lua_gettop(state) != 3) {
      return luaL_error(state, "'mcm.resource' takes 3 arguments, got %d", lua_gettop(state));
    }
    luaL_argcheck(state, lua_istable(state, 2), 2, "must be a table");
    luaL_argcheck(state, lua_istable(state, 3), 3, "must be a table");

    if (!luaL_getmetafield(state, 3, resourceTypeMetaKey)) {
      return luaL_argerror(state, 3, "expect resource table");
    }
    auto maybeTypeId = getResourceType(state, -1);
    uint64_t typeId;
    KJ_IF_MAYBE(t, maybeTypeId) {
      typeId = *t;
    } else {
      return luaL_argerror(state, 3, "expect resource table");
    }
    lua_pop(state, 1);

    auto& libState = getStateRef(state);
    auto res = libState.newResource();
    KJ_IF_MAYBE(id, getId(state, 1)) {
      res.setId(id->getValue());
      res.setComment(id->getComment());
    } else if (lua_isstring(state, 1)) {
      auto comment = luaStringPtr(state, 1);
      res.setId(idHash(comment));
      res.setComment(comment);
    } else {
      return luaL_argerror(state, 1, "expect mcm.hash or string");
    }
    lua_len(state, 2);
    lua_Integer ndeps = lua_tointeger(state, -1);
    lua_pop(state, 1);
    if (ndeps > 0) {
      auto depList = res.initDependencies(ndeps);
      // TODO(soon): sort
      for (lua_Integer i = 1; i <= ndeps; i++) {
        lua_geti(state, 2, i);
        KJ_IF_MAYBE(id, getId(state, -1)) {
          depList.set(i-1, id->getValue());
        } else if (lua_isstring(state, -1)) {
          depList.set(i-1, idHash(luaStringPtr(state, -1)));
        } else {
          return luaL_argerror(state, 2, "expect deps to contain only mcm.hash or strings");
        }
        lua_pop(state, 1);
      }
    }

    switch (typeId) {
    case 0:
      res.setNoop();
      break;
    case fileResId:
      {
        auto f = res.initFile();
        auto maybeExc = kj::runCatchingExceptions([state, &f]() {
          copyStruct(state, f);
        });
        KJ_IF_MAYBE(e, maybeExc) {
          pushLua(state, *e);
          return lua_error(state);
        }
      }
      break;
    case execResId:
      {
        auto e = res.initExec();
        auto maybeExc = kj::runCatchingExceptions([state, &e]() {
          copyStruct(state, e);
        });
        KJ_IF_MAYBE(e, maybeExc) {
          pushLua(state, *e);
          return lua_error(state);
        }
      }
      break;
    default:
      return luaL_argerror(state, 3, "unknown resource type");
    }
    return 0;
  }

  const luaL_Reg mcmlib[] = {
    {"exec", execfunc},
    {"file", filefunc},
    {"hash", hashfunc},
    {"resource", resourcefunc},
    {NULL, NULL},
  };

  int openmcm(lua_State* state) {
    luaL_newlib(state, mcmlib);

    lua_newtable(state);
    lua_createtable(state, 0, 1);  // new metatable
    pushResourceType(state, 0);
    lua_setfield(state, -2, resourceTypeMetaKey);  // metatable[resourceTypeMetaKey] = TOP
    lua_setmetatable(state, -2);  // pop metatable
    lua_setfield(state, -2, "noop");  // mcm.noop = TOP
    return 1;
  }
}  // namespace

Resource::Builder LibState::newResource() {
  auto orphan = scratch.getOrphanage().newOrphan<Resource>();
  auto builder = orphan.get();
  resources.add(kj::mv(orphan));
  return builder;
}

void openlib(lua_State *state, LibState& lib) {
  lua_pushlightuserdata(state, &lib);
  lua_setfield(state, LUA_REGISTRYINDEX, stateRefRegistryKey);
  luaL_requiref(state, "mcm", openmcm, 0);  // pushes module onto the stack
}

}  // namespace luacat
}  // namespace mcm
