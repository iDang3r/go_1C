package models

type User struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gortm:"size:50;not null"`
}

type Post struct {
	ID       uint   `gorm:"primaryKey"`
	Title    string `gortm:"size:100;not null"`
	Body     string `gorm:"type:text;not null"`
	Author   User
	AuthorID uint      `gorm:"not null"`
	Comments []Comment `gorm:"foreignKey:PostRefer"`
}

type Comment struct {
	ID        uint `gorm:"primaryKey"`
	PostRefer uint `gorm:"not null"`
	Author    User
	AuthorID  uint   `gorm:"not null"`
	Body      string `gorm:"type:text;not null"`
}
