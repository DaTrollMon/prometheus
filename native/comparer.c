// Copyright 2013 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include "comparer.hpp"
#include "comparer.h"

int compare(void *unused, const char *l, size_t llen,
            const char *r, size_t rlen) {
  return Compare(l, llen, r, rlen);
}

void destroy(void *unused) {
}

const char *name(void *unused) {
  return COMPARATOR_NAME;
}

leveldb_comparator_t* new_comparator() {
  return leveldb_comparator_create(NULL, &destroy, &compare, &name);
}
