'use strict'
const mysql = require('mysql2')

class Raw {
  constructor(value) {
    this.value = value
  }
}

class Mysql {
  #conn
  #database
  #defining = false
  #table = []
  #arr = {}
  #stack = []
  #statements = ['=', '>', '>=', '<', '<=', '!=', 'LIKE', 'NOT LIKE', 'IN', 'NOT IN', 'BETWEEN', 'NOT BETWEEN', 'IS', 'IS NOT']

  constructor(table, conn) {
    this.#conn = conn
    this.#stack = new Error().stack.split('\n').splice(3)
    if (table) {
      this.#arr.table = table
      this.#define(table)
      this.#defining = 1
      while (this.#defining && this.#defining < 255) {
        this.#defining++
      }
    }
  }

  //     function having(){
  //       if(count(func_get_args()) == 1 && !is_array(func_get_args()[0])){
  //         $this->arr['having'] = is_numeric(func_get_args()[0]) ? "id='".func_get_args()[0]."'" : "";
  //       }elseif(count(func_get_args()) > 0){
  //         $this->arr['having'] = isset($this->arr['having']) && trim($this->arr['having'])!='' ? $this->arr['having'].' AND '.$this->whereExtract(func_get_args()) : $this->whereExtract(func_get_args());
  //       }
  //       return new static($this->table,$this->arr);
  //     }

  //     function whereJson($col,$val){
  //       //return 'JSON_SEARCH('.$col.', "one", "'.$val.'") IS NOT NULL';
  //       return new static($this->table,$this->arr);
  //     }
  //     function cache($t=3600){
  //       if(!is_numeric($t)){
  //         $exp = explode(' ',str_replace('  ',' ',$t));
  //         if($exp[1] == 'second') $t = intval(trim($exp[0]));
  //         if($exp[1] == 'minute') $t = intval(trim($exp[0])) * 60;
  //         if($exp[1] == 'hour')   $t = intval(trim($exp[0])) * 60 * 60;
  //         if($exp[1] == 'day')    $t = intval(trim($exp[0])) * 60 * 60 * 24;
  //         if($exp[1] == 'week')   $t = intval(trim($exp[0])) * 60 * 60 * 24 * 7;
  //         if($exp[1] == 'month')  $t = intval(trim($exp[0])) * 60 * 60 * 24 * 30;
  //         if($exp[1] == 'year')   $t = intval(trim($exp[0])) * 60 * 60 * 24 * 365;
  //       }
  //       $this->arr['cache'] = $t;
  //       return new static($this->table,$this->arr);
  //     }

  async #define(table) {
    return new Promise(resolve => {
      if (!Odac.Mysql.db[this.#database]) Odac.Mysql.db[this.#database] = {}
      this.#table[table] = Odac.Mysql.db[this.#database][table]
      if (this.#table[table]) {
        this.#defining = false
        return resolve(true)
      }
      let columns = []
      this.#conn.query(`SHOW COLUMNS FROM ${this.escape(table, 'table')}`, (err, result) => {
        if (err) {
          this.#error(err)
          this.#defining = false
          return resolve(false)
        }
        for (let get of result) {
          columns[get.Field] = get
          if (get.Key == 'PRI') {
            if (!this.#table[table]) this.#table[table] = {}
            this.#table[table].primary = get.Field
          }
        }
        if (!this.#table[table]) this.#table[table] = {}
        this.#table[table].columns = columns
        Odac.Mysql.db[this.#database][table] = this.#table[table]
        this.#defining = false
        return resolve(true)
      })
    })
  }

  async delete() {
    let query = this.query('delete')
    let run = await this.run(query)
    if (run === false) return false
    this.affected = run.affectedRows
    //       if($this->affected > 0) self::clearcache();
    return this
  }

  #error(err, query) {
    err = 'Odac Mysql Error: ' + (err?.message ?? 'Unknown error').trim() + '\n'
    if (query) err += 'Query: ' + query + '\n'
    while (this.#stack.length > 0) {
      let line = this.#stack.shift().replace('at', '')
      if (line.includes('/node_modules/odac/framework/src/')) break
      else if (!line.includes('(node:')) err += line + '\n'
    }
    console.error(err)
    return false
  }

  escape(v, type) {
    if (!type) type = 'value'
    if (v && v instanceof Raw) return ' ' + v.value + ' '
    if (type == 'value') {
      if (v === null) return 'NULL'
      if (typeof v === 'object')
        return (
          ' (' +
          Object.values(v)
            .map(val => mysql.escape(val))
            .join(',') +
          ') '
        )
      return `${mysql.escape(v)}`
    } else if (type == 'table' || type == 'col') {
      let as = ''
      if (typeof v === 'object') {
        as = Object.values(v)[0]
        v = Object.keys(v)[0]
        as = type == 'col' ? ` AS ${mysql.escapeId(as)} ` : ` ${mysql.escapeId(as)} `
      }
      if (v.includes('.')) {
        return (
          v
            .split('.')
            .map(val => mysql.escapeId(val))
            .join('.') + as
        )
      }
      return mysql.escapeId(v) + as
    } else if (type == 'statement' || type == 'st') {
      return this.#statements.includes(v.toUpperCase()) ? v.toUpperCase() : '='
    }
  }

  first(b = false) {
    return new Promise((resolve, reject) => {
      this.#arr.limit = 1
      this.get(b)
        .then(sql => {
          if (sql === false || !sql[0]) return resolve(false)
          return resolve(sql[0])
        })
        .catch(reject)
    })
  }

  async get(b) {
    if (!b) b = false
    let data = []
    // if(isset($this->arr['cache'])){
    //   $md5_query = md5($query);
    //   $md5_table = md5($this->arr['table']);
    //   $file = "cache/mysql/".md5(Mysql::$name)."/$md5_table"."_$md5_query";
    //   $cache = Odac::storage($file)->get('cache');
    //   if(isset($cache->date) && ($cache->date >= (time() - $this->arr['cache']))) return $cache->data;
    // }
    let query = this.query('get')
    let sql = await this.run(query)
    //   console.log(sql);
    if (sql === false) return this.#error()
    for (let row of sql) {
      for (let [key, value] of Object.entries(row)) row[key] = await this.type(key, value)
      data.push(row)
    }
    // while($row = mysqli_fetch_assoc($sql)){
    //   foreach($row as $key => $value) $row[$key] = $this->type($key, $value);
    //   $data[] = $b ? $row : (object)$row;
    // }
    // mysqli_free_result($sql);
    // if(isset($cache)){
    //   $cache->data = $data;
    //   $cache->date = time();
    //   Odac::storage($file)->set('cache', $cache);
    // }
    return data
  }

  async insert(arr) {
    this.id = 1
    let ext = await this.#valuesExtract(arr)
    this.#arr['into'] = ext['into']
    this.#arr['values'] = ext['values']
    let query = this.query('insert')
    let run = await this.run(query)
    if (run === false) return false
    this.id = run.insertId
    this.affected = run.affectedRows
    // if(this.affected > 0) this.clearcache();
    return this
  }

  insertIgnore(arr) {
    this.#arr.ignore = true
    return this.insert(arr)
  }

  order(v1, v2 = 'asc') {
    // if(is_array($v1) && (!isset($v1['ct']) || $v1['ct'] != $GLOBALS['candy_token_mysql'])){
    //   $order = [];
    //   foreach($v1 as $key => $val)
    //   if(!is_int($key)) $order[] = $this->escape($key,'col').(strtolower($val) == 'desc' ? ' DESC' : ' ASC');
    //   else $order[] = $this->escape($val,'col').' ASC';
    //   $this->arr['order by'] = implode(',',$order);
    /* }else */ this.#arr['order by'] = this.escape(v1, 'col') + (v2.toLowerCase() == 'desc' ? ' DESC' : ' ASC')
    return this
  }

  orWhere(...args) {
    this.#arr.where =
      this.#arr.where && this.#arr.where.trim() != '' ? `${this.#arr.where} OR ${this.#whereExtract(args)}` : this.#whereExtract(args)
    return this
  }

  async replace(arr) {
    //       $this->id = 1;
    let ext = await this.#valuesExtract(arr)
    this.#arr['into'] = ext['into']
    this.#arr['values'] = ext['values']
    let query = this.query('replace')
    let run = await this.run(query)
    if (run === false) return false
    this.id = run.insertId
    this.affected = run.affectedRows
    //       if($sql === false) return $this->error();
    //       $this->success = $sql;
    //       self::clearcache();
    return this
  }

  async rows() {
    //       if(isset($this->arr['cache'])){
    //         $md5_query = md5($query);
    //         $md5_table = md5($this->arr['table']);
    //         $file = "cache/mysql/".md5(Mysql::$name)."/$md5_table"."_$md5_query"."_r";
    //         $cache = Odac::storage($file)->get('cache');
    //         if(isset($cache->date) && ($cache->date >= (time() - $this->arr['cache']))) return $cache->data;
    //       }
    let query = this.query('get')
    let sql = await this.run(query)
    if (sql === false) return this.#error()
    let rows = sql.length
    //       if(isset($cache)){
    //         $cache->data = $rows;
    //         $cache->date = time();
    //         Odac::storage($file)->set('cache', $cache);
    //       }
    return rows
  }

  run(query, params) {
    return new Promise(resolve => {
      if (!query) return resolve(false)
      if (!this.#conn) return resolve(false)
      if (this.#conn.state == 'disconnected') Odac.Mysql.init()
      const args = params ? [query, params] : [query]
      args.push((err, result) => {
        if (err) return resolve(this.#error(err, query))
        return resolve(result)
      })
      this.#conn.query(...args)
    })
  }

  select(...args) {
    this.#arr['select'] = this.#arr['select'] ?? []
    if (args.length == 1 && (typeof args[0] === 'object' || args[0] instanceof Raw)) {
      if (args[0] instanceof Raw) {
        this.#arr['select'].push(args[0].value)
      } else {
        for (let key of Object.keys(args[0])) {
          let value = args[0][key]
          if (isNaN(key)) this.#arr['select'].push(this.escape(key, 'col') + ' AS ' + this.escape(value, 'col'))
          else this.#arr['select'].push(this.escape(value, 'col'))
        }
      }
    } else {
      for (let key of args) this.#arr['select'].push(this.escape(key, 'col'))
    }
    return this
  }

  async set(arr, val) {
    let vars = ''
    if (!['array', 'object'].includes(typeof arr) && val !== undefined)
      vars += this.escape(arr, 'col') + ' = ' + this.escape(await this.type(arr, val, 'encode')) + ','
    else
      for (let [key, value] of Object.entries(arr))
        vars += this.escape(key, 'col') + ' = ' + this.escape(await this.type(key, value, 'encode')) + ','
    this.#arr.set = vars.substring(0, vars.length - 1)
    let query = this.query('set')
    let run = await this.run(query)
    if (run === false) return this.#error()
    this.affected = run.affectedRows
    if (this.affected > 0) this.#clearcache()
    return this
  }

  groupBy(...args) {
    this.#arr['group by'] = this.#arr['group by'] ?? ''
    let select = this.#arr['group by'] ? this.#arr['group by'].split(',') : []
    //       if(count(func_get_args())==1 && is_array(func_get_args()[0])){
    //         if(isset(func_get_args()[0]['ct']) && isset(func_get_args()[0]['v']) && func_get_args()[0]['ct'] == $GLOBALS['candy_token_mysql']){
    //           $select[] = func_get_args()[0]['v'];
    //         }else{
    //           foreach(func_get_args()[0] as $key => $value){
    //             $select[] = $this->escape($value,'col');
    //           }
    //         }
    /*       } else */ for (let key of args) select.push(this.escape(key, 'col'))
    this.#arr['group by'] = select.join(', ')
    return this
  }

  limit(v1, v2 = null) {
    this.#arr['limit'] = v2 === null ? v1 : `${v1}, ${v2}`
    return this
  }

  leftJoin(tb, col1, st = null, col2 = null) {
    return this.join(tb, col1, st, col2, 'left join')
  }

  rightJoin(tb, col1, st = null, col2 = null) {
    return this.join(tb, col1, st, col2, 'right join')
  }

  join(tb, col1, st = null, col2 = null, type = 'inner join') {
    this.#arr[type] = this.#arr[type] ?? []
    this.#define(Array.isArray(tb) ? Object.keys(tb)[0] : tb)
    tb = this.escape(tb, 'table')
    let state
    if (st === null && col2 === null) {
      col1 = this.#whereExtract(col1)
      col2 = ''
      state = ''
    } else {
      col1 = this.escape(col1, 'col')
      col2 = this.escape(col2 !== null ? col2 : st, 'col')
      state = this.escape(col2 !== null ? st : '=', 'st')
    }
    this.#arr[type].push(`${tb} ON ${col1} ${state} ${col2}`)
    return this
  }

  #clearcache() {
    //       if(!isset($this->arr['table'])) return false;
    //       $md5_table = md5($this->arr['table']);
    //       $file = "storage/cache/mysql/".md5(Mysql::$name)."/$md5_table*";
    //       foreach(glob($file) as $key) unlink($key);
    return true
  }

  query(type = 'get') {
    const arr_q = ['inner join', 'right join', 'left join', 'where', 'group by', 'having', 'order by', 'limit']
    let query = ''
    for (let key of arr_q) {
      if (this.#arr[key]) {
        if (Array.isArray(this.#arr[key])) {
          query += ' ' + key.toUpperCase() + ' ' + this.#arr[key].join(' ' + key.toUpperCase() + ' ')
        } else {
          query += ' ' + key.toUpperCase() + ' '
          query += this.#arr[key]
        }
      }
    }
    switch (type) {
      case 'get':
        query = `SELECT ${this.#arr.select ? this.#arr.select.join(', ') : '*'} FROM ${this.escape(this.#arr.table, 'table')} ${query}`
        break
      case 'set':
        query = `UPDATE ${this.escape(this.#arr['table'], 'table')} SET ${this.#arr['set']} ${query}`
        break
      case 'insert':
        query = `INSERT ${this.#arr.ignore ? 'IGNORE' : ''} INTO ${this.escape(this.#arr.table, 'table')} ${this.#arr.into} VALUES ${this.#arr.values}`
        break
      case 'delete':
        query = `DELETE FROM ${this.escape(this.#arr.table, 'table')} ${query}`
        break
      case 'replace':
        query = `REPLACE INTO ${this.escape(this.#arr.table, 'table')} ${this.#arr.into} VALUES ${this.#arr.values}`
        break
    }
    return query
  }

  async type(col, value, action = 'decode') {
    if (!this.types) this.types = {}
    if (!this.types[col]) {
      this.types[col] = 'string'
      for (const key of Object.keys(this.#table)) {
        if (!this.#table[key]) await this.#define(key)
        if (!this.#table[key]) throw new Error(`Table ${key} not found`)
        if (!this.#arr.select && this.#table[key].columns[col]?.Type) {
          this.types[col] = this.#table[key].columns[col]?.Type ?? this.types[col]
          break
        } else if (!this.#arr.select) {
          continue
        } else if (Odac.Var(this.#arr.select).contains(' AS "' + col + '"')) {
          // $exp = explode(' ,',explode(" AS \"$col\"",$this->arr['select'])[0]);
          //       $real_col = explode('.',Odac::var(trim(end($exp)))->clear('`'));
          //       $real_table = trim($real_col[0]);
          //       $real_col = trim($real_col[1]);
          //       $this->types[$col] = $this->types[$col] = $this->table[$real_table]['columns'][$real_col]['Type'] ?? $this->types[$col];
          break
        } else if (Odac.Var(this.#arr.select).containsAny(' `' + col + '`', ' `' + key + '`.`' + col + '`')) {
          this.types[col] = this.#table[key].columns[col].Type ?? this.types[col]
        }
      }
    }
    if (action == 'decode') {
      if (Odac.Var(this.types[col]).isBegin('tinyint(1)')) value = value ? true : false
      else if (Odac.Var(this.types[col]).isBegin('int')) value = parseInt(value)
      else if (Odac.Var(this.types[col]).isBegin('double')) value = parseFloat(value)
      else if (Odac.Var(this.types[col]).isBegin('float')) value = parseFloat(value)
      else if (Odac.Var(this.types[col]).isBegin('boolean')) value = parseInt(value)
      else if (Odac.Var(this.types[col]).isBegin('json')) value = JSON.parse(value)
    } else if (!(value instanceof Raw)) {
      if (Odac.Var(this.types[col]).isBegin('tinyint(1)')) value = parseInt(value)
      else if (Odac.Var(this.types[col]).isBegin('int')) value = parseInt(value)
      else if (Odac.Var(this.types[col]).isBegin('double')) value = parseFloat(value)
      else if (Odac.Var(this.types[col]).isBegin('float')) value = parseFloat(value)
      else if (Odac.Var(this.types[col]).isBegin('boolean')) value = parseInt(value)
      else if (Odac.Var(this.types[col]).isBegin('json')) value = JSON.stringify(value)
      else if (Odac.Var(this.types[col]).isBegin('date', 'datetime', 'timestamp')) value = Odac.Var(value).date('Y-m-d H:i:s')
    }
    return value
  }

  async #valuesExtract(arr) {
    let query_key = []
    let query_val = []
    let multiple = false
    let keys = Object.keys(arr)
    for (let i = 0; i < keys.length; i++) {
      let key = keys[i]
      let val = arr[key]
      //     if(is_object($val)) $val = (array)$val;
      //     if(is_array($val) && !isset($this->table[$this->arr['table']]['columns'][$key]) && (!isset($val['ct']) || $val['ct']!=$GLOBALS['candy_token_mysql'])){
      //       $multiple = true;
      //       $ex = $this->valuesExtract($val);
      //       $query_key = $ex['into'];
      //       $query_val[] = $ex['values'];
      /*     }else */ if (val === null) {
        query_key.push(this.escape(key, 'col'))
        query_val.push('NULL')
      } else {
        query_key.push(this.escape(key, 'col'))
        query_val.push(this.escape(await this.type(key, val, 'encode')))
      }
    }
    return {
      into: !multiple ? `(${query_key.join(',')})` : query_key,
      values: !multiple ? `(${query_val.join(',')})` : query_val.join(',')
    }
  }

  where(...args) {
    if (args.length == 1 && typeof args[0] !== 'object' && !(args[0] instanceof Raw)) {
      this.#arr.where = this.#whereExtract([this.#table[this.#arr.table].primary, args[0]])
    } else if (args.length > 0) {
      this.#arr.where =
        this.#arr.where && this.#arr.where.trim() != '' ? `${this.#arr.where} AND ${this.#whereExtract(args)}` : this.#whereExtract(args)
    }
    return this
  }

  #whereExtract(arr) {
    let q = ''
    let loop = 1
    let in_arr = false
    let state = '='
    let last = 0
    for (const key of arr) {
      if (key && Array.isArray(key) && state != 'IN' && state != 'NOT IN' && !(key instanceof Raw)) {
        q += last == 1 ? ' AND ' + this.#whereExtract(key) : this.#whereExtract(key)
        in_arr = true
        last = 1
      } else if (arr.length == 2 && loop == 2) {
        q += ' = ' + this.escape(key)
      } else if (in_arr) {
        q += key.toUpperCase() == 'OR' ? ' OR ' : ' AND '
        last = 2
      } else if (arr.length == 3 && loop == 2) {
        state = this.#statements.includes(key.toUpperCase()) ? key.toUpperCase() : '='
        q += ' ' + state
        last = 1
      } else if (key === null) {
        q += ' NULL '
      } else {
        q += this.escape(key, loop == 1 ? 'table' : 'value')
        last = 1
      }
      loop++
    }
    return `(${q})`
  }
}

module.exports = {
  conn: {},
  db: {},
  init: function () {
    return new Promise(resolve => {
      if (!Odac.Config.database) return resolve(false)
      let multiple = typeof Odac.Config.database[Object.keys(Odac.Config.database)[0]] === 'object'
      let dbs = multiple ? Odac.Config.database : {default: Odac.Config.database}
      for (let key of Object.keys(dbs)) {
        let db = dbs[key]
        if (db.type && db.type != 'mysql') continue
        Odac.Mysql.conn[key] = mysql.createConnection({
          host: db.host ?? '127.0.0.1',
          user: db.user,
          password: db.password,
          database: db.database,
          stringifyObjects: true
        })
        Odac.Mysql.conn[key].connect(err => {
          if (err) {
            console.error(`Odac Mysql Error: Failed to connect to database '${key}'`)
            console.error(`Host: ${db.host ?? '127.0.0.1'}`)
            console.error(`User: ${db.user}`)
            console.error(`Database: ${db.database}`)
            console.error(`Error: ${err.message}`)
            return resolve(false)
          }
        })
        Odac.Mysql.conn[key].query('SHOW TABLES', (err, result) => {
          if (err) {
            console.error(`Odac Mysql Error: Failed to query tables from database '${key}'`)
            console.error(`Error: ${err.message}`)
            return resolve(false)
          }
          for (let table of result)
            for (let key of Object.keys(table)) {
              let t = () => {
                new Mysql(table[key], Odac.Mysql.conn['default'])
              }
              t()
            }
        })
      }
      return resolve(true)
    })
  },
  database: function (name) {
    if (!Odac.Mysql.conn[name]) return null
    return new Mysql(name, Odac.Mysql.conn[name])
  },
  run: function (query, params) {
    if (!Odac.Mysql.conn['default']) return Promise.resolve(false)
    return new Mysql(null, Odac.Mysql.conn['default']).run(query, params)
  },
  table: function (name) {
    if (!Odac.Mysql.conn['default']) return null
    return new Mysql(name, Odac.Mysql.conn['default'])
  },
  raw: function (query) {
    if (typeof query !== 'string') {
      throw new Error('Mysql.raw() requires a string parameter')
    }
    return new Raw(query)
  }
}
