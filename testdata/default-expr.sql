# Test edge cases of column defaults and default expressions in MariaDB 10.2+
# and MySQL 8.0.13+.

SET foreign_key_checks=0;
SET sql_log_bin=0;

use testing

CREATE TABLE testdefaults (
	pk varchar(100) NOT NULL,
	a int default 4,
	b int default (a*a),
	c int default (abs(a)),
	d varchar(100) default 'hello',
	e varchar(200) default (concat(d, ' world')),
	f varchar(30) NOT NULL default '',
	g float default (rand()),
	h float default (rand() * rand()),
	i date default (current_date + interval 1 month),
	j blob,
	k datetime default current_timestamp,
	l timestamp default current_timestamp(),
	m timestamp(4) default current_timestamp(4),
	n text default (concat(d, ' worlds')),
	PRIMARY KEY (pk)
) ENGINE=InnoDB DEFAULT CHARSET=latin1;

